//go:build !darwin

// mobile 是 iOS 线的 c-archive 入口（CoastStart/CoastStop/CoastSuspend/
// CoastResume/CoastNextLog…），由 clashauto 仓库的 ios/core/build_libcoast.sh
// 编成 libcoast-ios.a / libcoast-sim.a 链进 NEPacketTunnelProvider。
//
// ★ 真正的实现全在带 //go:build darwin 约束的文件里（ios 隐含 darwin tag）——
//
//	这个包只属于 iOS 线。本文件反过来只在**非 darwin** 平台编译，是**故意**的：
//	c-archive 入口必须是 package main（所以不能用一个 package mobile 的 doc 文件——
//	会撞包名），而没有这个桩的话，非 darwin 平台上包里一个文件都不剩，
//	`go build ./...`/`go test ./...` 的 CI 腿会因
//	「build constraints exclude all Go files」而红。空 main 桩让它在那些平台上
//	只是一个平凡的可编译单元，什么都不做。
//
// ★ 源头（编辑处）是 clashauto 的 ios/core/ —— build_libcoast.sh 每次构建都会
//
//	整目录覆盖拷贝到这里。要改就改那边，改这边的会在下一次构建时被覆盖掉。
package main

func main() {}
