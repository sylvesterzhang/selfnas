#!/bin/sh
# selfnas：外接盘自动挂载 / 卸载。root 执行，由 systemd 拉起（见 selfnas-mount@.service）。
#
# 用法：selfnas-mount.sh add sdb1     插入时挂载
#       selfnas-mount.sh remove sdb1  拔出时清理
#
# 为什么不是 fstab：fstab 只在开机那一刻挂。运行中插进来的盘，local-fs.target
# 早已 active，systemd 不会为它重试；而 selfnas 以普通用户运行，既不能 mkdir /mnt
# 也不能 mount。所以把挂载交给 root，程序只负责「看」。
#
# 为什么不是 udev 里直接 mount：systemd-udevd 带 PrivateMounts=yes，它做的挂载
# 留在自己的私有命名空间里，外面根本看不见。udev 只负责点火，动作交给服务。

set -u
ACTION="${1:-}"
NAME="${2:-}"
if [ -z "$ACTION" ] || [ -z "$NAME" ]; then
	exit 0
fi

DEV="/dev/$NAME"
OWNER_UID=1000
OWNER_GID=1000

# 已挂载设备的挂载点（没有就空）
cur_mount() {
	awk -v d="$DEV" '$1==d{print $2; exit}' /proc/mounts 2>/dev/null
}

unmount_dev() {
	mnt=$(cur_mount)
	[ -z "$mnt" ] && return 0
	umount "$mnt" 2>/dev/null || umount -l "$mnt" 2>/dev/null
	rmdir "$mnt" 2>/dev/null
	logger -t selfnas "外接盘已卸载 $DEV（$mnt）"
}

case "$ACTION" in
remove)
	unmount_dev
	exit 0
	;;
add) ;;
*) exit 0 ;;
esac

# 设备可能已经没了（开机冷插拔的竞态）
[ -b "$DEV" ] || exit 0

# 已经挂好就别动
[ -n "$(cur_mount)" ] && exit 0

FSTYPE=$(blkid -s TYPE -o value "$DEV" 2>/dev/null)
[ -z "$FSTYPE" ] && exit 0
[ "$FSTYPE" = "swap" ] && exit 0
# 只碰外接盘常见的几种；ext4/xfs 是系统盘，误挂上来只会添乱
case "$FSTYPE" in
exfat | vfat | ntfs | ntfs3 | msdos | hfsplus | apfs) ;;
*) exit 0 ;;
esac

# 卷标做目录名：空格和斜杠换成 -，跟程序里 autoMountHook 的口径保持一致
LABEL=$(blkid -s LABEL -o value "$DEV" 2>/dev/null)
[ -z "$LABEL" ] && LABEL="$NAME"
DIR="/mnt/$(printf '%s' "$LABEL" | tr ' /\\' '---')"

mkdir -p "$DIR" 2>/dev/null || exit 0

# 挂载点被别的设备（或已拔掉的残影）占着 → 先摘掉，否则会挂到旧挂载上面去
if mountpoint -q "$DIR" 2>/dev/null; then
	other=$(awk -v m="$DIR" '$2==m{print $1; exit}' /proc/mounts 2>/dev/null)
	[ "$other" = "$DEV" ] && exit 0
	umount -l "$DIR" 2>/dev/null
fi

case "$FSTYPE" in
exfat | vfat | ntfs | ntfs3 | msdos)
	# 让挂载用户能读写：否则文件属主是 root，selfnas 连列目录都费劲
	mount -t "$FSTYPE" -o "uid=$OWNER_UID,gid=$OWNER_GID,umask=0022" "$DEV" "$DIR" 2>/dev/null ||
		mount -t "$FSTYPE" -o "uid=$OWNER_UID,gid=$OWNER_GID" "$DEV" "$DIR" 2>/dev/null
	;;
*)
	mount "$DEV" "$DIR" 2>/dev/null
	;;
esac

# 有些发行版把 exfat-fuse / ntfs-3g 装成独立命令，没注册成 mount 的 helper
if ! mountpoint -q "$DIR" 2>/dev/null; then
	case "$FSTYPE" in
	exfat)
		command -v mount.exfat-fuse >/dev/null 2>&1 &&
			mount.exfat-fuse -o "uid=$OWNER_UID,gid=$OWNER_GID" "$DEV" "$DIR" 2>/dev/null
		;;
	ntfs)
		command -v ntfs-3g >/dev/null 2>&1 &&
			ntfs-3g -o "uid=$OWNER_UID,gid=$OWNER_GID" "$DEV" "$DIR" 2>/dev/null
		;;
	esac
fi

if mountpoint -q "$DIR" 2>/dev/null; then
	logger -t selfnas "外接盘已挂载 $DEV（$FSTYPE）→ $DIR"
else
	# 挂不上就把空目录收掉，别在 /mnt 里留垃圾
	rmdir "$DIR" 2>/dev/null
	logger -t selfnas "外接盘挂载失败 $DEV（$FSTYPE）→ $DIR"
fi
