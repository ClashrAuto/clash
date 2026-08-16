//go:build !windows

package sing_tun

import (
	"fmt"
	"net"
	"syscall"
	"time"
)

// app 那侧 accept 之后立刻就发，10 秒是给「核心比 app 先到」留的余量。
// 太短会在慢机器上偶发失败，太长会让「app 根本没在监听」这种配置错误挂很久才报出来。
const fdSocketTimeout = 10 * time.Second

// receiveTunFileDescriptor 从一个 unix socket 上收下 tun 的文件描述符（SCM_RIGHTS）。
//
// ★★ 为什么需要这条路：Android 上 tun 的 fd 是 **VpnService 建的**，只能由 app 交给核心。
// 而 Coast 把核心跑成**独立子进程**，`tun.file-descriptor` 里那个数字在子进程里毫无意义 ——
// Android 的 ProcessBuilder（AOSP 的 java_lang_ProcessImpl）在 exec 前会把子进程里
// 3 号以上的 fd 全部关掉，清 FD_CLOEXEC 也救不回来（实测：`/proc/<app>/fd/127 -> /dev/tun`，
// 而 `/proc/<core>/fd/127` 根本不存在）。
//
// 失败形态非常安静：隧道建得起来、界面显示已连接、REST 正常、节点测速正常，只是核心那侧
// 一直刷 `read tun: bad file descriptor`，**没有一个包被代理**。
//
// 上游 ClashMetaForAndroid 遇不到这件事：它把核心作为 Go 库跑在 app 进程内，fd 直接可用。
//
// addr 支持 Linux 的抽象命名空间（以 `@` 开头，Go 的 net 包按 NUL 前缀处理），
// Android 上用抽象命名空间可以免掉文件系统路径与权限问题。
func receiveTunFileDescriptor(addr string) (int, error) {
	d := net.Dialer{Timeout: fdSocketTimeout}
	c, err := d.Dial("unix", addr)
	if err != nil {
		return 0, fmt.Errorf("dial %s: %w", addr, err)
	}
	defer c.Close()
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("%s is not a unix connection", addr)
	}
	// ★ 一定要设读超时：对端是 app，它要是没按约定发过来，没有超时的话核心就**永远停在这里**，
	//   而外面看到的是「点了连接没反应」而不是一条错误。
	if err := uc.SetReadDeadline(time.Now().Add(fdSocketTimeout)); err != nil {
		return 0, fmt.Errorf("set deadline: %w", err)
	}
	// 至少要收一个字节的正文，否则内核不会把附带的控制消息交上来
	buf := make([]byte, 1)
	oob := make([]byte, syscall.CmsgSpace(4))
	_, oobn, _, _, err := uc.ReadMsgUnix(buf, oob)
	if err != nil {
		return 0, fmt.Errorf("read from %s: %w", addr, err)
	}
	scms, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return 0, fmt.Errorf("parse control message: %w", err)
	}
	if len(scms) == 0 {
		return 0, fmt.Errorf("no control message from %s", addr)
	}
	fds, err := syscall.ParseUnixRights(&scms[0])
	if err != nil {
		return 0, fmt.Errorf("parse unix rights: %w", err)
	}
	if len(fds) == 0 {
		return 0, fmt.Errorf("no file descriptor from %s", addr)
	}
	// 多给了就只留第一个，其余立刻关掉 —— 泄漏出去的话谁都不会发现
	for _, extra := range fds[1:] {
		_ = syscall.Close(extra)
	}
	return fds[0], nil
}
