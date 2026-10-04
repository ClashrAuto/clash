package mmdb

import (
	"encoding/binary"

	"os"
	"path/filepath"
	"testing"
	"time"

	C "github.com/ClashrAuto/coast/constant"

	"github.com/oschwald/maxminddb-golang"
)

// testMMDB 拼一份最小的合法 MaxMind DB：一个节点的搜索树（两边都指「查无」）、空数据段、
// 元数据里的 build_epoch 用来分辨是哪一份。
func testMMDB(epoch uint64) []byte {
	str := func(s string) []byte { return append([]byte{0x40 | byte(len(s))}, s...) }
	u16 := func(v uint16) []byte { return []byte{0xa0 | 2, byte(v >> 8), byte(v)} }
	u32 := func(v uint32) []byte {
		b := binary.BigEndian.AppendUint32(nil, v)
		return append([]byte{0xc0 | 4}, b...)
	}
	u64 := func(v uint64) []byte { // 扩展类型 9：控制字节的类型位为 0，下一字节是 9-7
		b := binary.BigEndian.AppendUint64(nil, v)
		return append([]byte{8, 2}, b...)
	}
	meta := []byte{0xe0 | 9}
	add := func(k string, v []byte) { meta = append(append(meta, str(k)...), v...) }
	add("node_count", u32(1))
	add("record_size", u16(24))
	add("ip_version", u16(4))
	add("database_type", str("Coast-Test"))
	add("languages", []byte{0x00, 0x04}) // 空数组（扩展类型 11）
	add("binary_format_major_version", u16(2))
	add("binary_format_minor_version", u16(0))
	add("build_epoch", u64(epoch))
	add("description", []byte{0xe0}) // 空 map

	var b []byte
	b = append(b, 0, 0, 1, 0, 0, 1) // 一个节点，左右记录都 = node_count（查无）
	b = append(b, make([]byte, 16)...)
	b = append(b, "\xab\xcd\xefMaxMind.com"...)
	return append(b, meta...)
}

// 与 Swift 线 MmdbFile.applyStaged 一样：先写到旁边，再改名换上（核心手里 mmap 的旧 inode 不受影响）。
func swapIn(t *testing.T, path string, data []byte) {
	t.Helper()
	tmp := path + ".new"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func resetIPState(t *testing.T) {
	t.Helper()
	ReloadIP()
	ipReader = IPReader{}
	oldHome := C.Path.HomeDir()
	oldDelay, oldClose := reopenCloseDelay, closeOldReader
	t.Cleanup(func() {
		ReloadIP()
		ipReader = IPReader{}
		C.SetHomeDir(oldHome)
		reopenCloseDelay, closeOldReader = oldDelay, oldClose
	})
}

func TestReopenIPPicksUpSwappedFile(t *testing.T) {
	resetIPState(t)
	dir := t.TempDir()
	C.SetHomeDir(dir)
	path := filepath.Join(dir, "Country.mmdb")
	swapIn(t, path, testMMDB(1))
	reopenCloseDelay = 100 * time.Millisecond
	closed := make(chan *maxminddb.Reader, 1)
	closeOldReader = func(r *maxminddb.Reader) { closed <- r }

	if got := IPInstance().Metadata.BuildEpoch; got != 1 {
		t.Fatalf("初次加载 build_epoch=%d，应为 1", got)
	}
	old := IPInstance().Reader

	swapIn(t, path, testMMDB(2))
	if got := IPInstance().Metadata.BuildEpoch; got != 1 {
		t.Fatalf("没调 ReopenIP 就变成了 %d —— 这条用例的前提（加载一次就不再读盘）不成立了", got)
	}
	if !ReopenIP() {
		t.Fatal("已加载时 ReopenIP 应返回 true")
	}
	if got := IPInstance().Metadata.BuildEpoch; got != 2 {
		t.Fatalf("ReopenIP 之后 build_epoch=%d，应为新换上的 2", got)
	}

	// 旧库延后关闭：不能当场关（正在查的协程会撞上撤掉的 mmap），也不能不关（每换一次漏一份映射）。
	select {
	case <-closed:
		t.Fatal("旧库当场就关了")
	default:
	}
	select {
	case r := <-closed:
		if r != old {
			t.Fatal("关掉的不是被换下的那份")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("延后期过了旧库还没关")
	}
}

func TestReopenIPBeforeLoadIsNoop(t *testing.T) {
	resetIPState(t)
	dir := t.TempDir()
	C.SetHomeDir(dir)
	swapIn(t, filepath.Join(dir, "Country.mmdb"), testMMDB(3))
	if ReopenIP() {
		t.Fatal("还没加载过时 ReopenIP 应什么都不做")
	}
	if got := IPInstance().Metadata.BuildEpoch; got != 3 {
		t.Fatalf("build_epoch=%d，应为 3", got)
	}
}

// 换上的文件打不开时保留旧库：IPInstance 打不开是 log.Fatalln，重置了就等于把核心打死。
func TestReopenIPKeepsLoadedWhenFileBroken(t *testing.T) {
	resetIPState(t)
	dir := t.TempDir()
	C.SetHomeDir(dir)
	path := filepath.Join(dir, "Country.mmdb")
	swapIn(t, path, testMMDB(4))
	if got := IPInstance().Metadata.BuildEpoch; got != 4 {
		t.Fatalf("build_epoch=%d，应为 4", got)
	}
	swapIn(t, path, []byte("not a mmdb"))
	if ReopenIP() {
		t.Fatal("坏文件不该被换上")
	}
	if got := IPInstance().Metadata.BuildEpoch; got != 4 {
		t.Fatalf("坏文件之后 build_epoch=%d，应仍为 4", got)
	}
}
