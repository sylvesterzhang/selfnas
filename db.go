package main

import (
	"database/sql"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// 「最近」和「收藏」存在服务端的 SQLite 里，跟着账号走 —— 换手机、换浏览器都还在。
// 之前存在浏览器 localStorage，本质上只属于那一台设备的那个浏览器，跟账号无关。
//
// 表里**只存 root + path + 时间**，文件名/大小/类型一律读的时候现 stat：
//   · 文件被改名、被删、被移走，读出来就是「已不在」，不会留一条骗人的旧记录；
//   · 存副本的话得考虑同步，两份数据早晚会打架。

var (
	db     *sql.DB
	dbFile string
)

// StoredEntry 一条记录。path 相对根目录、用 / 分隔。
type StoredEntry struct {
	Root      string `json:"root"`
	RootName  string `json:"rootName"`
	RootDrive string `json:"rootDrive"`
	Path      string `json:"path"`
	Name      string `json:"name"`
	IsDir     bool   `json:"isDir"`
	Kind      string `json:"kind"`
	Size      int64  `json:"size"`
	Missing   bool   `json:"missing"`
	TS        int64  `json:"ts"`
}

// 表名走白名单，避免把参数直接拼进 SQL
const (
	tblRecent = "recent"
	tblFav    = "favorite"
)

func tableOK(t string) bool { return t == tblRecent || t == tblFav }

func initDB(dir string) error {
	return openDBAt(filepath.Join(dir, "selfnas.db"))
}

// openDBAt 直接指定数据库文件。
// 之所以要能指定：自动化测试会往「最近 / 收藏」里灌数据、还会清场，
// 如果共用正式库，跑一次测试等于把用户的数据抹一遍（2026-10-27 真发生过）。
// 测试用 -db 指向独立文件，从此跟正式库物理隔离。
func openDBAt(file string) error {
	dbFile = file
	dsn := "file:" + filepath.ToSlash(dbFile) +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	// 这个量级（每人几十行）不需要连接池；单连接把「写并发」这件麻烦事直接消掉
	d.SetMaxOpenConns(1)
	if err = d.Ping(); err != nil {
		_ = d.Close()
		return err
	}
	db = d
	return migrateDB()
}

func migrateDB() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS recent (
			username TEXT NOT NULL,
			root     TEXT NOT NULL,
			path     TEXT NOT NULL,
			ts       INTEGER NOT NULL,
			PRIMARY KEY (username, root, path)
		)`,
		`CREATE TABLE IF NOT EXISTS favorite (
			username TEXT NOT NULL,
			root     TEXT NOT NULL,
			path     TEXT NOT NULL,
			ts       INTEGER NOT NULL,
			PRIMARY KEY (username, root, path)
		)`,
		`CREATE INDEX IF NOT EXISTS ix_recent_ts ON recent (username, ts DESC)`,
		`CREATE INDEX IF NOT EXISTS ix_fav_ts ON favorite (username, ts DESC)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return err
		}
	}
	return nil
}

func rootNameOf(rootPath string) string {
	for _, r := range cfg.Roots {
		if strings.EqualFold(normPath(r.Path), normPath(rootPath)) {
			return r.Name
		}
	}
	return ""
}

// listStored 读列表并按当前磁盘情况补齐元信息
func listStored(table, user string, limit int) ([]StoredEntry, error) {
	if !tableOK(table) {
		return nil, sql.ErrNoRows
	}
	rows, err := db.Query(
		`SELECT root, path, ts FROM `+table+` WHERE username = ? ORDER BY ts DESC LIMIT ?`,
		user, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []StoredEntry{}
	for rows.Next() {
		var e StoredEntry
		if err = rows.Scan(&e.Root, &e.Path, &e.TS); err != nil {
			return nil, err
		}
		e.RootName = rootNameOf(e.Root)
		e.RootDrive = driveOf(e.Root)
		e.Name = filepath.Base(filepath.FromSlash(e.Path))
		e.Kind = fileKind(e.Name, false)
		full, _, rerr := resolve(e.Root, e.Path)
		if rerr != nil {
			e.Missing = true
		} else if st, serr := os.Stat(full); serr != nil {
			e.Missing = true
		} else {
			e.IsDir = st.IsDir()
			e.Kind = fileKind(e.Name, st.IsDir())
			if !st.IsDir() {
				e.Size = st.Size()
			}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// flush 把 WAL 立刻合并进主库并截断。
//
// 这里不做批处理、也不攒着定期刷：表只有几十行，一次 checkpoint 是毫秒级，
// 但换来的是「进程被强杀也不丢」这个确定性。之前的教训是数据先躺在 WAL 里，
// 一脚踩空（checkpoint 中途被 TerminateProcess）就再也读不出来了。
func flush() {
	if db == nil {
		return
	}
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		log.Printf("[数据库] checkpoint 失败: %v", err)
	}
}

// upsertStored 同一个文件再看一次只是把时间刷新，不是新增一条
func upsertStored(table, user, root, path string) error {
	if !tableOK(table) {
		return sql.ErrNoRows
	}
	if _, err := db.Exec(
		`INSERT INTO `+table+` (username, root, path, ts) VALUES (?, ?, ?, ?)
		 ON CONFLICT(username, root, path) DO UPDATE SET ts = excluded.ts`,
		user, root, path, time.Now().Unix()); err != nil {
		return err
	}
	if table == tblRecent {
		// 只留最近 60 条：再多也不叫「最近」了，而且列表本来只显示前 30
		if _, err := db.Exec(
			`DELETE FROM recent WHERE username = ? AND rowid NOT IN (
				SELECT rowid FROM recent WHERE username = ? ORDER BY ts DESC LIMIT 60)`, user, user); err != nil {
			return err
		}
	}
	flush()
	return nil
}

func removeStored(table, user, root, path string) error {
	if !tableOK(table) {
		return sql.ErrNoRows
	}
	if _, err := db.Exec(`DELETE FROM `+table+` WHERE username = ? AND root = ? AND path = ?`, user, root, path); err != nil {
		return err
	}
	flush()
	return nil
}

func clearStored(table, user string) error {
	if !tableOK(table) {
		return sql.ErrNoRows
	}
	if _, err := db.Exec(`DELETE FROM `+table+` WHERE username = ?`, user); err != nil {
		return err
	}
	flush()
	return nil
}

func isFavorited(user, root, path string) bool {
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(1) FROM favorite WHERE username = ? AND root = ? AND path = ?`,
		user, root, path).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

func closeDB() {
	if db == nil {
		return
	}
	// 关闭前把 WAL 全量合并进主库并截断。
	//
	// 不这么做的后果（2026-10-27 真踩过）：WAL 模式下数据先写进 -wal，
	// 主库要等 checkpoint 才更新。如果进程在 checkpoint 中途被强杀，
	// 主库可能停在「改了一半」，而下一次启动时 WAL 里那批帧会被判定为
	// 「已经应用到主库的旧帧」而整体忽略 —— 已提交的记录就这么没了。
	// 显式 TRUNCATE 一次，主库自己就是完整的，下次启动不依赖 WAL 重放。
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		log.Printf("[数据库] checkpoint 失败: %v", err)
	}
	if err := db.Close(); err != nil {
		log.Printf("[数据库] 关闭失败: %v", err)
	}
}
