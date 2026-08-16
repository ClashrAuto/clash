//go:build windows

package sing_tun

import "errors"

// Windows 上没有 SCM_RIGHTS，也没有「app 建 tun 交给核心」这种形态（那是 Android 专属的）。
// 留一个明确失败的实现，而不是让 `file-descriptor-socket` 被静默忽略 ——
// 静默忽略的话核心会退回去建自己的 tun，与调用方的预期完全不同且不报错。
func receiveTunFileDescriptor(addr string) (int, error) {
	return 0, errors.New("file-descriptor-socket is not supported on windows")
}
