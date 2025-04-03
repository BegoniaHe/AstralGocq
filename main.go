// Package main
package main

import (
	"github.com/BegoniaHe/AstralGocq/cmd/gocq"
	"github.com/BegoniaHe/AstralGocq/global/terminal"

	_ "github.com/BegoniaHe/AstralGocq/db/leveldb"   // leveldb 数据库支持
	_ "github.com/BegoniaHe/AstralGocq/modules/silk" // silk编码模块
	// 其他模块
	// _ "github.com/BegoniaHe/AstralGocq/db/sqlite3"   // sqlite3 数据库支持
	// _ "github.com/BegoniaHe/AstralGocq/db/mongodb"    // mongodb 数据库支持
	// _ "github.com/BegoniaHe/AstralGocq/modules/pprof" // pprof 性能分析
)

func main() {
	terminal.SetTitle()
	gocq.InitBase()
	gocq.PrepareData()
	gocq.LoginInteract()
	_ = terminal.DisableQuickEdit()
	_ = terminal.EnableVT100()
	gocq.WaitSignal()
	_ = terminal.RestoreInputMode()
}
