//go:build android && !cmfa

package sing_tun

import (
	"errors"
	"sync"

	"github.com/ClashrAuto/coast/component/process"
	"github.com/ClashrAuto/coast/constant"
	"github.com/ClashrAuto/coast/constant/features"
	"github.com/ClashrAuto/coast/log"

	"github.com/metacubex/sing-tun"
)

type packageManagerCallback struct{}

func (cb *packageManagerCallback) OnPackagesUpdated(packageCount int, sharedCount int) {}

func newPackageManager() (tun.PackageManager, error) {
	packageManager, err := tun.NewPackageManager(tun.PackageManagerOptions{
		Callback: &packageManagerCallback{},
		Logger:   log.SingLogger,
	})
	if err != nil {
		return nil, err
	}
	err = packageManager.Start()
	if err != nil {
		return nil, err
	}
	return packageManager, nil
}

var (
	globalPM tun.PackageManager
	pmOnce   sync.Once
	pmErr    error
)

func getPackageManager() (tun.PackageManager, error) {
	pmOnce.Do(func() {
		globalPM, pmErr = newPackageManager()
	})
	return globalPM, pmErr
}

// ★★ 拿不到包列表**不是**致命错误，除非用户真的配了按包分流。
//
// 包管理器读的是 /data/system/packages.xml，那是 root 才有的权限 —— 也就是说在
// **每一台普通手机上**它都失败。而这个错误以前会一路冒到 server.go:475 的
// `build android rules`，让整个 TUN 入站起不来。
//
// 那种失败形态极其安静：VPN 隧道是 app 用 VpnService 建的，系统状态栏有钥匙图标、
// 界面显示「已连接」、核心的 REST 一切正常，只是**没有任何一个包被代理** ——
// 写进 tun fd 的数据没人读。日志里那一行 error 是唯一的迹象。
//
// 这张列表只服务于 include-package / exclude-package 两个选项。Coast 的按应用分流
// 走的是 Java 侧的 VpnService.addAllowedApplication/addDisallowedApplication，核心
// 根本用不上它；`find-process-mode: off` 时 findPackageName 也不会被调到。
// 所以没配那两个选项就跳过，配了才按错误处理（否则会静默按「全放行」跑，
// 那比起不来更糟 —— 用户以为某些应用被排除了，其实没有）。
//
// 上游 ClashMetaForAndroid 用 `cmfa` 构建标签整个换掉这个文件，包列表由 Java 侧喂进去；
// 我们的 android 产物只带 `with_gvisor`，所以走的是这条路。
func (l *Listener) buildAndroidRules(tunOptions *tun.Options) error {
	packageManager, err := getPackageManager()
	if err != nil {
		if len(tunOptions.IncludePackage) > 0 || len(tunOptions.ExcludePackage) > 0 {
			return err
		}
		log.Warnln("[TUN] 读不到 Android 包列表（非 root 上属正常）：%s；未配置 include-package / exclude-package，跳过按包分流规则", err)
		return nil
	}
	tunOptions.BuildAndroidRules(packageManager, l.handler)
	return nil
}

func findPackageName(metadata *constant.Metadata) (string, error) {
	packageManager, err := getPackageManager()
	if err != nil {
		return "", err
	}
	uid := metadata.Uid
	if sharedPackage, loaded := packageManager.SharedPackageByID(uid % 100000); loaded {
		return sharedPackage, nil
	}
	if packageName, loaded := packageManager.PackageByID(uid % 100000); loaded {
		return packageName, nil
	}
	return "", errors.New("package not found")
}

func init() {
	if !features.CMFA {
		process.DefaultPackageNameResolver = findPackageName
	}
}
