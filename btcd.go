// Copyright (c) 2013-2016 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"net"
	"net/http"
	//  执行 包内 init 函数 （net 相关 性能统计）
	_ "net/http/pprof"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/pprof"


	"github.com/btcsuite/btcd/blockchain/indexers"
	"github.com/btcsuite/btcd/database"
	"github.com/btcsuite/btcd/limits"
)

const (
	// blockDbNamePrefix is the prefix for the block database name.  The
	// database type is appended to this value to form the full block
	// database name.
	blockDbNamePrefix = "blocks"
)

var (
	cfg *config
)

// winServiceMain is only invoked on Windows.  It detects when btcd is running
// as a service and reacts accordingly.
var winServiceMain func() (bool, error)

// btcdMain is the real main function for btcd.  It is necessary to work around
// the fact that deferred functions do not run when os.Exit() is called.  The
// optional serverChan parameter is mainly used by the service code to be
// notified with the server once it is setup so it can gracefully stop it when
// requested from the service control manager.


func btcdMain(serverChan chan<- *server) error {
	// Load configuration and parse command line.  This function also
	// initializes logging and configures it accordingly.
	tcfg, _, err := loadConfig()
	if err != nil {
		return err
	}
	cfg = tcfg
	defer func() {
		if logRotator != nil {
			logRotator.Close()
		}
	}()

	// Get a channel that will be closed when a shutdown signal has been
	// triggered either from an OS signal such as SIGINT (Ctrl+C) or from
	// another subsystem such as the RPC server.
	// 处理 退出信号（系统信号） 只是给出交互提示，为真正关闭service，监听器
	interrupt := interruptListener()
	defer btcdLog.Info("Shutdown complete")

	// Show version at startup.
	btcdLog.Infof("Version %s", version())

	// Enable http profiling server if requested.
	// 如果http要求，处理此配置  看不懂
	if cfg.Profile != "" {
		go func() {
			listenAddr := net.JoinHostPort("", cfg.Profile) // IP":"PORT
			btcdLog.Infof("Profile server listening on %s", listenAddr)
			profileRedirect := http.RedirectHandler("/debug/pprof",
				http.StatusSeeOther)
			http.Handle("/", profileRedirect)
			btcdLog.Errorf("%v", http.ListenAndServe(listenAddr, nil))
		}()
	}

	// Write cpu profile if requested.
	// CPU 设置 （如果设置了 cpu  则，统计性能）
	if cfg.CPUProfile != "" {
		f, err := os.Create(cfg.CPUProfile)  // 应该类似句柄
		if err != nil {
			btcdLog.Errorf("Unable to create cpu profile: %v", err)
			return err
		}
		pprof.StartCPUProfile(f)
		defer f.Close()
		defer pprof.StopCPUProfile()
	}

	// Perform upgrades to btcd as new versions require it.
	// 判断升级 数据目录迁移  数据库协议升级
	// dranm   /Users/peng/Library/Application Support/Btcd  数据目录
	if err := doUpgrades(); err != nil {
		btcdLog.Errorf("%v", err)
		return err
	}

	// Return now if an interrupt signal was triggered.
	// 启用service时，检测一下 处理 终端信号（Control + C） （应该是检测，另有协程（interrupt）在监测）
	if interruptRequested(interrupt) {
		return nil
	}

	// Load the block database.
	// 加载数据库（memdb、cfg.DbType）
	db, err := loadBlockDB()
	if err != nil {
		btcdLog.Errorf("%v", err)
		return err
	}

	// 最后执行（延迟执行）
	defer func() {
		// Ensure the database is sync'd and closed on shutdown.
		btcdLog.Infof("Gracefully shutting down the database...")
		// 关闭数据库
		db.Close()
	}()

	// Return now if an interrupt signal was triggered.
	// 检测 service 状态
	if interruptRequested(interrupt) {
		return nil
	}

	// Drop indexes and exit if requested.
	//
	// NOTE: The order is important here because dropping the tx index also
	// drops the address index since it relies on it.
	// 	DropAddrIndex        bool          `long:"dropaddrindex" description:"Deletes the address-based transaction index from the database on start up and then exits."`
	//  还没太理解，为什么要 drop
	if cfg.DropAddrIndex {
		if err := indexers.DropAddrIndex(db, interrupt); err != nil {
			btcdLog.Errorf("%v", err)
			return err
		}

		return nil
	}
	if cfg.DropTxIndex {
		if err := indexers.DropTxIndex(db, interrupt); err != nil {
			btcdLog.Errorf("%v", err)
			return err
		}

		return nil
	}
	if cfg.DropCfIndex {
		if err := indexers.DropCfIndex(db, interrupt); err != nil {
			btcdLog.Errorf("%v", err)
			return err
		}

		return nil
	}

	// https://www.jianshu.com/p/d3667b026938  简书 分析 btcd

	// Create server and start it.
	// 创建server and start it
	// 参数： 监听地址（？？？？  应该是服务（协程）），   数据库 ，  参数  ， 终端信号处理协程
	// 服务初始化
	server, err := newServer(cfg.Listeners, db, activeNetParams.Params,
		interrupt)
	if err != nil {
		// TODO: this logging could do with some beautifying.
		// 启动失败
		btcdLog.Errorf("Unable to start server on %v: %v",
			cfg.Listeners, err)
		return err
	}

	// 最后执行（延迟执行）（server停止后的提示）
	defer func() {
		btcdLog.Infof("Gracefully shutting down the server...")
		server.Stop()
		server.WaitForShutdown()
		srvrLog.Infof("Server shutdown complete")
	}()

	// 服务启动
	server.Start()
	if serverChan != nil {
		// 仅仅是接受  在调用的地方  ，有可能处理？？？？
		serverChan <- server
	}

	// Wait until the interrupt signal is received from an OS signal or
	// shutdown is requested through one of the subsystems such as the RPC
	// server.

	// 停止 两种方式 1、OS signal  2、子服务请求（  RPC server  哪个是？？？）
	// channel 这种输出没有接受？？？？
	<-interrupt
	return nil
}

// removeRegressionDB removes the existing regression test database if running
// in regression test mode and it already exists.
func removeRegressionDB(dbPath string) error {
	// Don't do anything if not in regression test mode.
	if !cfg.RegressionTest {
		return nil
	}

	// Remove the old regression test database if it already exists.
	fi, err := os.Stat(dbPath)
	if err == nil {
		btcdLog.Infof("Removing regression test database from '%s'", dbPath)
		if fi.IsDir() {
			err := os.RemoveAll(dbPath)
			if err != nil {
				return err
			}
		} else {
			err := os.Remove(dbPath)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// dbPath returns the path to the block database given a database type.
func blockDbPath(dbType string) string {
	// The database name is based on the database type.
	dbName := blockDbNamePrefix + "_" + dbType
	if dbType == "sqlite" {
		dbName = dbName + ".db"
	}
	dbPath := filepath.Join(cfg.DataDir, dbName)
	return dbPath
}

// warnMultipleDBs shows a warning if multiple block database types are detected.
// This is not a situation most users want.  It is handy for development however
// to support multiple side-by-side databases.
// 多类型数据库并存支持btc检测&& 警告（这是不允许的）
func warnMultipleDBs() {
	// This is intentionally not using the known db types which depend
	// on the database types compiled into the binary since we want to
	// detect legacy db types as well.
	dbTypes := []string{"ffldb", "leveldb", "sqlite"}
	duplicateDbPaths := make([]string, 0, len(dbTypes)-1)
	for _, dbType := range dbTypes {
		if dbType == cfg.DbType {
			continue
		}

		// Store db path as a duplicate db if it exists.
		dbPath := blockDbPath(dbType)
		if fileExists(dbPath) {
			duplicateDbPaths = append(duplicateDbPaths, dbPath)
		}
	}

	// Warn if there are extra databases.
	if len(duplicateDbPaths) > 0 {
		selectedDbPath := blockDbPath(cfg.DbType)
		btcdLog.Warnf("WARNING: There are multiple block chain databases "+
			"using different database types.\nYou probably don't "+
			"want to waste disk space by having more than one.\n"+
			"Your current database is located at [%v].\nThe "+
			"additional database is located at %v", selectedDbPath,
			duplicateDbPaths)
	}
}

// loadBlockDB loads (or creates when needed) the block database taking into
// account the selected database backend and returns a handle to it.  It also
// contains additional logic such warning the user if there are multiple
// databases which consume space on the file system and ensuring the regression
// test database is clean when in regression test mode.
func loadBlockDB() (database.DB, error) {
	// The memdb backend does not have a file path associated with it, so
	// handle it uniquely.  We also don't want to worry about the multiple
	// database type warnings when running with the memory database.
	if cfg.DbType == "memdb" {
		btcdLog.Infof("Creating block database in memory.")
		db, err := database.Create(cfg.DbType)
		if err != nil {
			return nil, err
		}
		return db, nil
	}

	// 多类型数据库并存支持btc检测&& 警告（这是不鼓励的，但不禁止）
	warnMultipleDBs()

	// The database name is based on the database type.
	// 获取数据库地址（目录或者文件）
	dbPath := blockDbPath(cfg.DbType)

	// The regression test is special in that it needs a clean database for
	// each run, so remove it now if it already exists.
	// 删除 测试 数据库（以防数据混乱？？？）
	removeRegressionDB(dbPath)

	btcdLog.Infof("Loading block database from '%s'", dbPath)
	db, err := database.Open(cfg.DbType, dbPath, activeNetParams.Net)
	if err != nil {
		// 数据库 err 处理 1、抛异常 2、不存在，则建立
		// Return the error if it's not because the database doesn't
		// exist.
		if dbErr, ok := err.(database.Error); !ok || dbErr.ErrorCode !=
			database.ErrDbDoesNotExist {

			return nil, err
		}

		// Create the db if it does not exist.
		err = os.MkdirAll(cfg.DataDir, 0700)
		if err != nil {
			return nil, err
		}
		db, err = database.Create(cfg.DbType, dbPath, activeNetParams.Net)
		if err != nil {
			return nil, err
		}
	}

	btcdLog.Info("Block database loaded")
	return db, nil
}

func main() {
	// Use all processor cores.
	runtime.GOMAXPROCS(runtime.NumCPU())

	// Block and transaction processing can cause bursty allocations.  This
	// limits the garbage collector from excessively overallocating during
	// bursts.  This value was arrived at with the help of profiling live
	// usage.
	debug.SetGCPercent(10)

	// Up some limits.
	if err := limits.SetLimits(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to set limits: %v\n", err)
		os.Exit(1)
	}

	// Call serviceMain on Windows to handle running as a service.  When
	// the return isService flag is true, exit now since we ran as a
	// service.  Otherwise, just fall through to normal operation.
	if runtime.GOOS == "windows" {
		isService, err := winServiceMain()
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		if isService {
			os.Exit(0)
		}
	}

	// Work around defer not working after os.Exit()
	// 为啥 btcdMain 函数 参数 是niL
	if err := btcdMain(nil); err != nil {
		os.Exit(1)
	}
}
