//go:build linux

package main

// Linux 的「磁盘发现」。Windows 首装扫盘符（C 盘之外全开、卷标做名字），
// 这里保持同一口径：
//   1) 家目录 —— Linux 没有盘符，家目录是最接近「用户自己的空间」的存在；
//   2) /media、/mnt 下已挂载的外部存储（U 盘、移动硬盘、NFS/CIFS），
//      挂载点路径当名字，系统挂载点与虚拟文件系统一律不扫。
// 用户之后可以在管理台增删、改名、收窄。

import (
	"encoding/json"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

func listDrives() []DriveInfo {
	var out []DriveInfo
	if hd, ok := homeDrive(); ok {
		out = append(out, hd)
	}
	if b, err := os.ReadFile("/proc/self/mountinfo"); err == nil {
		mounts := linuxExternalMounts(parseMountInfoLinux(string(b)))
		sort.Slice(mounts, func(i, j int) bool { return mounts[i].Path < mounts[j].Path })
		for _, m := range mounts {
			if st, err := os.Stat(m.Path); err != nil || !st.IsDir() {
				continue
			}
			out = append(out, DriveInfo{
				Path:  m.Path,
				Label: uniqueDriveName(filepath.Base(m.Path), out),
				Kind:  driveKindOf(m.FSType),
				Ready: true,
			})
		}
	}
	return out
}

// volumeSpaceOS 磁盘容量。Statfs 在 Linux/macOS 上字段含义一致：
// Blocks*frsize 是总容量，Bavail（非 root 保留后可用）*frsize 是剩余可用。
func volumeSpaceOS(path string) (uint64, uint64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0
	}
	bsize := uint64(st.Bsize)
	return uint64(st.Blocks) * bsize, uint64(st.Bavail) * bsize
}

// ---------- 自动挂载（「重新扫描磁盘」时调用，见 handleRootsRescan） ----------
//
// Windows/macOS 插盘即自动挂载，Linux 不是：fstab + nofail 的盘拔掉再插回
// 不会恢复挂载（没有 udev 自动挂载规则），挂载点只剩空目录，页面进去啥都没有。
// 这里把「有文件系统但没挂载」的块设备挂回来：
//   1) /etc/fstab 里有该盘的条目 → mount <目录>，沿用用户配的目录和选项；
//   2) 没有 → 兜底挂到 /mnt/<卷标或设备名>（目录已存在就直接复用）。
// 挂不动的（非 root、缺 helper）记日志跳过，不让一块盘卡住整个扫描。

type lsblkNode struct {
	Name       string      `json:"name"`
	FSType     string      `json:"fstype"`
	MountPoint string      `json:"mountpoint"`
	Label      string      `json:"label"`
	UUID       string      `json:"uuid"`
	Children   []lsblkNode `json:"children"`
}

func flattenLsblk(nodes []lsblkNode, out *[]lsblkNode) {
	for _, n := range nodes {
		*out = append(*out, n)
		flattenLsblk(n.Children, out)
	}
}

// fstabDirs 解析 /etc/fstab：key 形如 uuid:<uuid> / label:<label> / dev:<设备名>，value 是挂载目录。
func fstabDirs() map[string]string {
	m := map[string]string{}
	b, err := os.ReadFile("/etc/fstab")
	if err != nil {
		return m
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		src, dir := f[0], f[1]
		switch {
		case strings.HasPrefix(src, "UUID="):
			m["uuid:"+strings.TrimPrefix(src, "UUID=")] = dir
		case strings.HasPrefix(src, "LABEL="):
			m["label:"+strings.TrimPrefix(src, "LABEL=")] = dir
		default:
			m["dev:"+filepath.Base(src)] = dir
		}
	}
	return m
}

// unmountedPartitions 统计「有文件系统、但没挂上」的块设备数。
//
// 只用来把话说清楚：扫盘扫不到时不一定是没插盘，很多是「盘在、但没挂上」
// （Linux 不像 Windows 插盘即挂）。有数字就能指着去装自动挂载助手。
func unmountedPartitions() int {
	out, err := exec.Command("lsblk", "-Jno", "NAME,FSTYPE,MOUNTPOINT").Output()
	if err != nil {
		return 0
	}
	var tree struct {
		BlockDevices []lsblkNode `json:"blockdevices"`
	}
	if json.Unmarshal(out, &tree) != nil {
		return 0
	}
	var nodes []lsblkNode
	flattenLsblk(tree.BlockDevices, &nodes)
	n := 0
	for _, x := range nodes {
		if x.FSType == "" || x.FSType == "swap" || x.MountPoint != "" {
			continue
		}
		n++
	}
	return n
}

// autoMountHook 返回本次新挂载的目录列表。找不到 lsblk / 非 root 导致全失败时返回 nil。
func autoMountHook() []string {
	out, err := exec.Command("lsblk", "-Jno", "NAME,FSTYPE,MOUNTPOINT,LABEL,UUID").Output()
	if err != nil {
		log.Printf("[磁盘] 自动挂载: lsblk 失败: %v", err)
		return nil
	}
	var tree struct {
		BlockDevices []lsblkNode `json:"blockdevices"`
	}
	if err := json.Unmarshal(out, &tree); err != nil {
		log.Printf("[磁盘] 自动挂载: lsblk 输出解析失败: %v", err)
		return nil
	}
	var nodes []lsblkNode
	flattenLsblk(tree.BlockDevices, &nodes)
	fstab := fstabDirs()

	var mounted []string
	for _, n := range nodes {
		if n.FSType == "" || n.FSType == "swap" || n.MountPoint != "" {
			continue
		}
		dev := "/dev/" + n.Name
		dir, via := "", ""
		if d, ok := fstab["uuid:"+n.UUID]; ok {
			dir, via = d, "fstab"
		} else if n.Label != "" {
			if d, ok := fstab["label:"+n.Label]; ok {
				dir, via = d, "fstab"
			}
		}
		if dir == "" {
			if d, ok := fstab["dev:"+n.Name]; ok {
				dir, via = d, "fstab"
			}
		}
		if dir == "" {
			base := n.Label
			if base == "" {
				base = n.Name
			}
			// 目录名只留安全字符，卷标里的空格/斜杠换成 -
			base = strings.Map(func(r rune) rune {
				if r == '/' || r == '\\' || r == ' ' || r == '\t' {
					return '-'
				}
				return r
			}, base)
			dir, via = "/mnt/"+base, "自动"
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Printf("[磁盘] 自动挂载: 建目录 %s 失败: %v", dir, err)
			continue
		}
		var merr error
		if via == "fstab" {
			merr = exec.Command("mount", dir).Run()
		} else {
			merr = exec.Command("mount", dev, dir).Run()
			// mount.exfat 可能没注册成 helper（Ubuntu 的 exfat-fuse 只装 mount.exfat-fuse），
			// 失败时按文件系统挑专用工具再试一次
			if merr != nil {
				switch n.FSType {
				case "exfat":
					if _, e := exec.LookPath("mount.exfat-fuse"); e == nil {
						merr = exec.Command("mount.exfat-fuse", dev, dir).Run()
					}
				case "ntfs":
					if _, e := exec.LookPath("ntfs-3g"); e == nil {
						merr = exec.Command("ntfs-3g", dev, dir).Run()
					}
				}
			}
		}
		if merr != nil {
			log.Printf("[磁盘] 自动挂载: %s(%s) → %s 失败: %v", dev, n.FSType, dir, merr)
			continue
		}
		log.Printf("[磁盘] 自动挂载: %s(%s) → %s (%s)", dev, n.FSType, dir, via)
		mounted = append(mounted, dir)
	}
	return mounted
}
