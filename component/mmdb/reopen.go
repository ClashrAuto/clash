package mmdb

import (
	"time"

	coastOnce "github.com/ClashrAuto/coast/common/once"
	C "github.com/ClashrAuto/coast/constant"
	"github.com/ClashrAuto/coast/log"

	"github.com/oschwald/maxminddb-golang"
)

// Coast 自有：app 换上新的 Country.mmdb 之后，让正在跑的核心改用它。
//
// ★★ IP 库用 sync.Once 加载一次就一直 mmap 着，上游只有 UpdateMMDB（核心自己下载）会重置它。
//   我们的 GeoIP 由 app 下载、校验、改名换上（Swift 线 MmdbFile.applyStaged），核心从头到尾
//   不知道；而 macOS 的系统扩展进程比隧道长命（跨多次 CoastStop/CoastStart 一直活着），
//   于是换上的新库在进程退出前一次都不会被读到（2026-10-04 真机：扩展进程活了 6 天，
//   期间换上过两份新库，分流一直用的是第一天那份）。

// reopenCloseDelay 是旧库延后关闭的时长，closeOldReader 是关它的动作（测试里都换掉）。
var (
	reopenCloseDelay = time.Minute
	closeOldReader   = func(r *maxminddb.Reader) { _ = r.Close() }
)

// ReopenIP 让下一次 IP 查询从磁盘重新打开 Country.mmdb。
// 还没加载过时什么都不做（下一次查询本来就会读新文件），返回 false。
//
// ★ 先确认新文件打得开：IPInstance 打不开时是 log.Fatalln，换上一份坏库等于把核心整个打死。
// ★ 旧库不能当场 Close：mmap 一撤，正在 Lookup 的协程直接 SIGBUS（上游 UpdateMMDB 先 Close
// 再重置，靠的是运气）。延后一分钟再关 —— 查询是微秒级的；文件早已被改名替换，
// 延后只是多映射一会儿。
func ReopenIP() bool {
	if !coastOnce.Done(&ipOnce) {
		return false
	}
	path := C.Path.MMDB()
	if !Verify(path) {
		log.Warnln("[GEO] %s can't be opened, keep using the loaded MMDB", path)
		return false
	}
	old := ipReader.Reader
	ReloadIP()
	if old != nil {
		time.AfterFunc(reopenCloseDelay, func() { closeOldReader(old) })
	}
	log.Infoln("[GEO] MMDB will be reopened from %s on next lookup", path)
	return true
}
