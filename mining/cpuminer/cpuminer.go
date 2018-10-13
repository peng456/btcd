// Copyright (c) 2014-2016 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package cpuminer

import (
	"errors"
	"fmt"
	"math/rand"
	"runtime"
	"sync"
	"time"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/mining"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
)

const (
	// maxNonce is the maximum value a nonce can be in a block header.
	maxNonce = ^uint32(0) // 2^32 - 1  ，in a block header

	// maxExtraNonce is the maximum value an extra nonce used in a coinbase
	// transaction can be.
	maxExtraNonce = ^uint64(0) // 2^64 - 1  in a coinbase transaction can be

	// hpsUpdateSecs is the number of seconds to wait in between each
	// update to the hashes per second monitor.
	hpsUpdateSecs = 10

	// hashUpdateSec is the number of seconds each worker waits in between
	// notifying the speed monitor with how many hashes have been completed
	// while they are actively searching for a solution.  This is done to
	// reduce the amount of syncs between the workers that must be done to
	// keep track of the hashes per second.
	hashUpdateSecs = 15
)

var (
	// defaultNumWorkers is the default number of workers to use for mining
	// and is based on the number of processor cores.  This helps ensure the
	// system stays reasonably responsive under heavy load.
	defaultNumWorkers = uint32(runtime.NumCPU())
)

// Config is a descriptor containing the cpu miner configuration.
type Config struct {
	// ChainParams identifies which chain parameters the cpu miner is
	// associated with.
	// 参数，CPU 相关.
	ChainParams *chaincfg.Params

	// BlockTemplateGenerator identifies the instance to use in order to
	// generate block templates that the miner will attempt to solve.
	// 块模板
	BlockTemplateGenerator *mining.BlkTmplGenerator

	// MiningAddrs is a list of payment addresses to use for the generated
	// blocks.  Each generated block will randomly choose one of them.
	// 矿工地址  （打包奖励、交易手续费  钱打入的地址）
	MiningAddrs []btcutil.Address

	// ProcessBlock defines the function to call with any solved blocks.
	// It typically must run the provided block through the same set of
	// rules and handling as any other block coming from the network.
	// 处理 来自网络的 其他块
	ProcessBlock func(*btcutil.Block, blockchain.BehaviorFlags) (bool, error)

	// ConnectedCount defines the function to use to obtain how many other
	// peers the server is connected to.  This is used by the automatic
	// persistent mining routine to determine whether or it should attempt
	// mining.  This is useful because there is no point in mining when not
	// connected to any peers since there would no be anyone to send any
	// found blocks to.
	ConnectedCount func() int32

	// IsCurrent defines the function to use to obtain whether or not the
	// block chain is current.  This is used by the automatic persistent
	// mining routine to determine whether or it should attempt mining.
	// This is useful because there is no point in mining if the chain is
	// not current since any solved blocks would be on a side chain and and
	// up orphaned anyways.
	IsCurrent func() bool
}

// CPUMiner provides facilities for solving blocks (mining) using the CPU in
// a concurrency-safe manner.  It consists of two main goroutines -- a speed
// monitor and a controller for worker goroutines which generate and solve
// blocks.  The number of goroutines can be set via the SetMaxGoRoutines
// function, but the default is based on the number of processor cores in the
// system which is typically sufficient.
type CPUMiner struct {
	sync.Mutex
	g                 *mining.BlkTmplGenerator
	cfg               Config
	numWorkers        uint32
	started           bool
	discreteMining    bool
	submitBlockLock   sync.Mutex
	wg                sync.WaitGroup
	workerWg          sync.WaitGroup
	updateNumWorkers  chan struct{}
	queryHashesPerSec chan float64
	updateHashes      chan uint64
	speedMonitorQuit  chan struct{}
	quit              chan struct{}
}


// CPU 挖矿 速度监控协程
// speedMonitor handles tracking the number of hashes per second the mining
// process is performing.  It must be run as a goroutine.
func (m *CPUMiner) speedMonitor() {
	log.Tracef("CPU miner speed monitor started")

	var hashesPerSec float64
	var totalHashes uint64

	// 定时器 10 秒
	ticker := time.NewTicker(time.Second * hpsUpdateSecs)
	defer ticker.Stop()  // 延时执行（定时器关闭）

	// select 语法讲解http://www.runoob.com/go/go-select-statement.html

out:   // 循环
	for {
		select { // 接受协程消息
		// Periodic updates from the workers with how many hashes they
		// have performed.
		// 定时更新 hash 数量
		case numHashes := <-m.updateHashes:
			totalHashes += numHashes

		// Time to update the hashes per second.
		// 每10秒执行下
		case <-ticker.C:

			curHashesPerSec := float64(totalHashes) / hpsUpdateSecs
			if hashesPerSec == 0 {
				hashesPerSec = curHashesPerSec
			}
			hashesPerSec = (hashesPerSec + curHashesPerSec) / 2
			totalHashes = 0
			if hashesPerSec != 0 {
				log.Debugf("Hash speed: %6.0f kilohashes/s",
					hashesPerSec/1000)
			}

		// Request for the number of hashes per second.
		// 把每秒hash数 通过 chnnel 告诉其他 协程
		case m.queryHashesPerSec <- hashesPerSec:
			// Nothing to do.

		//	停止信号
		case <-m.speedMonitorQuit:
			break out // 跳转到  out 处继续 循环 ？？？
		}
	}

	m.wg.Done()
	log.Tracef("CPU miner speed monitor done")
}

// submitBlock submits the passed block to network after ensuring it passes all
// of the consensus validation rules.
func (m *CPUMiner) submitBlock(block *btcutil.Block) bool {
	m.submitBlockLock.Lock()
	defer m.submitBlockLock.Unlock()

	// Ensure the block is not stale since a new block could have shown up
	// while the solution was being found.  Typically that condition is
	// detected and all work on the stale block is halted to start work on
	// a new block, but the check only happens periodically, so it is
	// possible a block was found and submitted in between.
	msgBlock := block.MsgBlock()
	if !msgBlock.Header.PrevBlock.IsEqual(&m.g.BestSnapshot().Hash) {
		log.Debugf("Block submitted via CPU miner with previous "+
			"block %s is stale", msgBlock.Header.PrevBlock)
		return false  //  ????
	}

	// Process this block using the same rules as blocks coming from other
	// nodes.  This will in turn relay it to the network like normal.
	isOrphan, err := m.cfg.ProcessBlock(block, blockchain.BFNone)
	if err != nil {
		// Anything other than a rule violation is an unexpected error,
		// so log that error as an internal error.
		if _, ok := err.(blockchain.RuleError); !ok {
			log.Errorf("Unexpected error while processing "+
				"block submitted via CPU miner: %v", err)
			return false
		}

		log.Debugf("Block submitted via CPU miner rejected: %v", err)
		return false
	}
	if isOrphan {
		log.Debugf("Block submitted via CPU miner is an orphan")
		return false
	}

	// The block was accepted.
	coinbaseTx := block.MsgBlock().Transactions[0].TxOut[0]
	log.Infof("Block submitted via CPU miner accepted (hash %s, "+
		"amount %v)", block.Hash(), btcutil.Amount(coinbaseTx.Value))
	return true
}

// solveBlock attempts to find some combination of a nonce, extra nonce, and
// current timestamp which makes the passed block hash to a value less than the
// target difficulty.  The timestamp is updated periodically and the passed
// block is modified with all tweaks during this process.  This means that
// when the function returns true, the block is ready for submission.
//
// This function will return early with false when conditions that trigger a
// stale block such as a new block showing up or periodically when there are
// new transactions and enough time has elapsed without finding a solution.

// 开始挖矿 算啊算啊
func (m *CPUMiner) solveBlock(msgBlock *wire.MsgBlock, blockHeight int32,
	ticker *time.Ticker, quit chan struct{}) bool {

	// Choose a random extra nonce offset for this block template and
	// worker.
	enOffset, err := wire.RandomUint64() // 64位 随机数（整数）
	if err != nil {
		log.Errorf("Unexpected error while generating random "+
			"extra nonce offset: %v", err)
		enOffset = 0
	}

	// Create some convenience variables.
	header := &msgBlock.Header
	// 转化数值形式，    CompactTobig    Big
	// targetDifficulty 目标难度
	targetDifficulty := blockchain.CompactToBig(header.Bits)

	// Initial state.
	lastGenerated := time.Now()
	// 交易池 最新 的时间（不知道是指什么时间）
	lastTxUpdate := m.g.TxSource().LastUpdated()
	// hash 开始下标  ？？？
	hashesCompleted := uint64(0)  // hash 更新次数

	// Note that the entire extra nonce range is iterated and the offset is
	// added relying on the fact that overflow will wrap around 0 as
	// provided by the Go spec.

	// 挖矿  最大计算次数  2^64 * 2^32   ？？？
	// extraNonce（范围 0 ~ 2^64）  双 sha 后 结果；maxExtraNonce 目标值(2^64 - 1)
	// extraNonce 从0开始递增   enOffset
	for extraNonce := uint64(0); extraNonce < maxExtraNonce; extraNonce++ {
		// Update the extra nonce in the block template with the
		// new value by regenerating the coinbase script and
		// setting the merkle root to the new value.
		// enOffset  随机 64位无符号整型
		// 改变块本身交易 使得默克尔树hash 改变（居然没有选择，添加交易 或者减少交易）
		// extraNonce+enOffset 交易记录内容？？？所以随机内容
		m.g.UpdateExtraNonce(msgBlock, blockHeight, extraNonce+enOffset)  // enOffset ???

		// Search through the entire nonce range for a solution while
		// periodically checking for early quit and stale block
		// conditions along with updates to the speed monitor.
		for i := uint32(0); i <= maxNonce; i++ {
			select {
			case <-quit:
				return false

			case <-ticker.C:
				// 计时器每15秒产生一次通讯
				m.updateHashes <- hashesCompleted
				hashesCompleted = 0

				// The current block is stale（过期） if the best block
				// has changed.
				best := m.g.BestSnapshot()
				if !header.PrevBlock.IsEqual(&best.Hash) {
					return false
				}

				// The current block is stale if the memory pool
				// has been updated since the block template was
				// generated and it has been at least one
				// minute.
				if lastTxUpdate != m.g.TxSource().LastUpdated() &&
					time.Now().After(lastGenerated.Add(time.Minute)) {

					return false
				}

				// 主网更新 这里只更新时间戳
				m.g.UpdateBlockTime(msgBlock)

			default:
				// Non-blocking select to fall through
			}

			// Update the nonce and hash the block header.  Each
			// hash is actually a double sha256 (two hashes), so
			// increment the number of hashes completed for each
			// attempt accordingly.
			header.Nonce = i  // 变换的随机值  来自上边
			hash := header.BlockHash() // 计算本块 hash 值
			hashesCompleted += 2   // 加2 啥意思？？？

			// The block is solved when the new block hash is less
			// than the target difficulty.  Yay!
			// 算出来了，尤里卡！！！
			if blockchain.HashToBig(&hash).Cmp(targetDifficulty) <= 0 {
				// 通知 其他 chain
				m.updateHashes <- hashesCompleted
				return true
			}
		}
	}

	return false
}

// generateBlocks is a worker that is controlled by the miningWorkerController.
// It is self contained in that it creates block templates and attempts to solve
// them while detecting when it is performing stale work and reacting
// accordingly by generating a new block template.  When a block is solved, it
// is submitted.
//
// It must be run as a goroutine.
func (m *CPUMiner) generateBlocks(quit chan struct{}) {
	log.Tracef("Starting generate blocks worker")

	// Start a ticker which is used to signal checks for stale work and
	// updates to the speed monitor.
	// 15 秒
	ticker := time.NewTicker(time.Second * hashUpdateSecs)
	defer ticker.Stop()
out:
	for {
		// Quit when the miner is stopped.
		select {
		case <-quit:  // 停止信号 则停止本循环 ，本矿工协程 停止工作
			break out
		default:
			// Non-blocking select to fall through
		}

		// Wait until there is a connection to at least one other peer
		// since there is no way to relay a found block or receive
		// transactions to work on when there are no connected peers.

		// 是否 连接其他 节点， 这是必须的，因为其他 节点会广播  交易  ，是挖矿 的 原料
		if m.cfg.ConnectedCount() == 0 {
			time.Sleep(time.Second)
			continue
		}

		// No point in searching for a solution before the chain is
		// synced.  Also, grab the same lock as used for block
		// submission, since the current block will be changing and
		// this would otherwise end up building a new block template on
		// a block that is in the process of becoming stale.
		m.submitBlockLock.Lock()
		curHeight := m.g.BestSnapshot().Height
		if curHeight != 0 && !m.cfg.IsCurrent() {
			m.submitBlockLock.Unlock()
			time.Sleep(time.Second)
			continue
		}

		// Choose a payment address at random. // 随机 选择 一个交易
		rand.Seed(time.Now().UnixNano())
		payToAddr := m.cfg.MiningAddrs[rand.Intn(len(m.cfg.MiningAddrs))]

		// Create a new block template using the available transactions
		// in the memory pool as a source of transactions to potentially
		// include in the block.
		template, err := m.g.NewBlockTemplate(payToAddr)
		m.submitBlockLock.Unlock()
		if err != nil {
			errStr := fmt.Sprintf("Failed to create new block "+
				"template: %v", err)
			log.Errorf(errStr)
			continue
		}

		// Attempt to solve the block.  The function will exit early
		// with false when conditions that trigger a stale block, so
		// a new block template can be generated.  When the return is
		// true a solution was found, so submit the solved block.
		if m.solveBlock(template.Block, curHeight+1, ticker, quit) {
			block := btcutil.NewBlock(template.Block)
			m.submitBlock(block)
		}
	}

	m.workerWg.Done()
	log.Tracef("Generate blocks worker done") // 提示 本矿工 歇菜，休息了
}

// miningWorkerController launches the worker goroutines that are used to
// generate block templates and solve them.  It also provides the ability to
// dynamically adjust the number of running worker goroutines.
//
// It must be run as a goroutine.

// 协程 必须是协程

// 矿工 && ==》 计算 and 生成 块
// 另外 提供 动态 调整 工作ing矿工协程 的能力

func (m *CPUMiner) miningWorkerController() {
	// launchWorkers groups common code to launch a specified number of
	// workers for generating blocks.
	var runningWorkers []chan struct{}  // 定义  ？？？矿工 用于通信 的 channel  数组

	// 初始化 矿工S
	// numWorkers 矿工数量
	launchWorkers := func(numWorkers uint32) {
		for i := uint32(0); i < numWorkers; i++ {
			quit := make(chan struct{}) // 管道 channel  ,用于通知其他矿工信息（"我挖到金子啦"等）
			runningWorkers = append(runningWorkers, quit) // 加入到数组

			m.workerWg.Add(1) // sync.WaitGroup
			// 挖矿协程
			go m.generateBlocks(quit)
		}
	}

	// Launch the current number of workers by default.
	runningWorkers = make([]chan struct{}, 0, m.numWorkers)  //  初始化  与上边对应？？？

	// 矿工开始挖矿 （m.numWorkers  个矿工）
	launchWorkers(m.numWorkers)

out:
	for {
		select {
		// Update the number of running workers.
		case <-m.updateNumWorkers:   // 接收此通道 信息，更新 全局矿工 数量 && ==> 维持 矿工协程 数量
			// No change.
			numRunning := uint32(len(runningWorkers))  // runningWorkers 这个 channel 数组 是 实时的
			if m.numWorkers == numRunning {
				continue
			}

			// Add new workers.
			if m.numWorkers > numRunning {
				launchWorkers(m.numWorkers - numRunning)
				continue
			}

			// 如果超过规定数量的矿工，则关闭多余 的矿工（协程）  关闭策略，把新生成的 矿工 关掉（其下标较大 （下标 依次递涨））
			// 猜测关闭 协程 方法 ，关闭 channel ,则协程收到信息 后 自己会自行关闭
			// Signal the most recently created goroutines to exit.
			for i := numRunning - 1; i >= m.numWorkers; i-- {
				close(runningWorkers[i])
				runningWorkers[i] = nil
				runningWorkers = runningWorkers[:i]
			}

		case <-m.quit:  // 检测这个 channel ，如果收到 关闭信号 ，则 会  依次 关闭矿工
			for _, quit := range runningWorkers {
				close(quit)
			}
			break out
		}
	}

	// Wait until all workers shut down to stop the speed monitor since
	// they rely on being able to send updates to it.
	m.workerWg.Wait()   // 协程数组处理，等所有协程关闭后 主协程 才关闭
	close(m.speedMonitorQuit) // 关闭 挖矿速率 监视协程
	m.wg.Done()  // 协程数组
}

// Start begins the CPU mining process as well as the speed monitor used to
// track hashing metrics.  Calling this function when the CPU miner has
// already been started will have no effect.
//
// This function is safe for concurrent access. 线程安全
func (m *CPUMiner) Start() {
	m.Lock()  // 加锁
	defer m.Unlock()  //延迟处理

	// Nothing to do if the miner is already running or if running in
	// discrete mode (using GenerateNBlocks).
	if m.started || m.discreteMining {
		// 如果已经开始  或  不挖矿，则返回
		return
	}

	// 定义 各种chan
	m.quit = make(chan struct{})
	m.speedMonitorQuit = make(chan struct{})
	m.wg.Add(2)  // 加入协程组 便于管理，告诉主协程 不退出，等待协程执行完成

	// 	重点研究 此处  挖矿 &&&&&  如何挖矿 再此处

	// 矿工挖矿速度监视器（及时调整）
	go m.speedMonitor()
	// 矿工 工人 控制 器 （应该在此处开始挖矿了吧？？？？）
	go m.miningWorkerController()

	m.started = true
	log.Infof("CPU miner started") // 提示 已经开始挖矿了
}

// Stop gracefully stops the mining process by signalling all workers, and the
// speed monitor to quit.  Calling this function when the CPU miner has not
// already been started will have no effect.
//
// This function is safe for concurrent access.
func (m *CPUMiner) Stop() {
	m.Lock()
	defer m.Unlock()

	// Nothing to do if the miner is not currently running or if running in
	// discrete mode (using GenerateNBlocks).
	if !m.started || m.discreteMining {
		return
	}

	close(m.quit)
	m.wg.Wait()
	m.started = false
	log.Infof("CPU miner stopped")
}

// IsMining returns whether or not the CPU miner has been started and is
// therefore currenting mining.
//
// This function is safe for concurrent access.
func (m *CPUMiner) IsMining() bool {
	m.Lock()
	defer m.Unlock()

	return m.started
}

// HashesPerSecond returns the number of hashes per second the mining process
// is performing.  0 is returned if the miner is not currently running.
//
// This function is safe for concurrent access.
func (m *CPUMiner) HashesPerSecond() float64 {
	m.Lock()
	defer m.Unlock()

	// Nothing to do if the miner is not currently running.
	if !m.started {
		return 0
	}

	return <-m.queryHashesPerSec
}

// SetNumWorkers sets the number of workers to create which solve blocks.  Any
// negative values will cause a default number of workers to be used which is
// based on the number of processor cores in the system.  A value of 0 will
// cause all CPU mining to be stopped.
//
// This function is safe for concurrent access.
func (m *CPUMiner) SetNumWorkers(numWorkers int32) {
	if numWorkers == 0 {
		m.Stop()
	}

	// Don't lock until after the first check since Stop does its own
	// locking.
	m.Lock()
	defer m.Unlock()

	// Use default if provided value is negative.
	if numWorkers < 0 {
		m.numWorkers = defaultNumWorkers
	} else {
		m.numWorkers = uint32(numWorkers)
	}

	// When the miner is already running, notify the controller about the
	// the change.
	if m.started {
		m.updateNumWorkers <- struct{}{}
	}
}

// NumWorkers returns the number of workers which are running to solve blocks.
//
// This function is safe for concurrent access.
func (m *CPUMiner) NumWorkers() int32 {
	m.Lock()
	defer m.Unlock()

	return int32(m.numWorkers)
}

// GenerateNBlocks generates the requested number of blocks. It is self
// contained in that it creates block templates and attempts to solve them while
// detecting when it is performing stale work and reacting accordingly by
// generating a new block template.  When a block is solved, it is submitted.
// The function returns a list of the hashes of generated blocks.
func (m *CPUMiner) GenerateNBlocks(n uint32) ([]*chainhash.Hash, error) {
	m.Lock()

	// Respond with an error if server is already mining.
	if m.started || m.discreteMining {
		m.Unlock()
		return nil, errors.New("Server is already CPU mining. Please call " +
			"`setgenerate 0` before calling discrete `generate` commands.")
	}

	m.started = true
	m.discreteMining = true

	m.speedMonitorQuit = make(chan struct{})
	m.wg.Add(1)
	go m.speedMonitor()

	m.Unlock()

	log.Tracef("Generating %d blocks", n)

	i := uint32(0)
	blockHashes := make([]*chainhash.Hash, n)

	// Start a ticker which is used to signal checks for stale work and
	// updates to the speed monitor.
	ticker := time.NewTicker(time.Second * hashUpdateSecs)
	defer ticker.Stop()

	for {
		// Read updateNumWorkers in case someone tries a `setgenerate` while
		// we're generating. We can ignore it as the `generate` RPC call only
		// uses 1 worker.
		// 有啥用？？？
		select {
		case <-m.updateNumWorkers:
		default:
		}

		// Grab the lock used for block submission, since the current block will
		// be changing and this would otherwise end up building a new block
		// template on a block that is in the process of becoming stale.
		m.submitBlockLock.Lock()
		curHeight := m.g.BestSnapshot().Height

		// Choose a payment address at random.
		rand.Seed(time.Now().UnixNano())
		payToAddr := m.cfg.MiningAddrs[rand.Intn(len(m.cfg.MiningAddrs))]

		// Create a new block template using the available transactions
		// in the memory pool as a source of transactions to potentially
		// include in the block.
		template, err := m.g.NewBlockTemplate(payToAddr)
		m.submitBlockLock.Unlock()
		if err != nil {
			errStr := fmt.Sprintf("Failed to create new block "+
				"template: %v", err)
			log.Errorf(errStr)
			continue
		}

		// Attempt to solve the block.  The function will exit early
		// with false when conditions that trigger a stale block, so
		// a new block template can be generated.  When the return is
		// true a solution was found, so submit the solved block.
		if m.solveBlock(template.Block, curHeight+1, ticker, nil) {
			block := btcutil.NewBlock(template.Block)
			m.submitBlock(block)
			blockHashes[i] = block.Hash()
			i++
			if i == n {
				log.Tracef("Generated %d blocks", i)
				m.Lock()
				close(m.speedMonitorQuit)
				m.wg.Wait()
				m.started = false
				m.discreteMining = false
				m.Unlock()
				return blockHashes, nil
			}
		}
	}
}

// New returns a new instance of a CPU miner for the provided configuration.
// Use Start to begin the mining process.  See the documentation for CPUMiner
// type for more details.
func New(cfg *Config) *CPUMiner {
	return &CPUMiner{
		g:                 cfg.BlockTemplateGenerator,
		cfg:               *cfg,
		numWorkers:        defaultNumWorkers,
		updateNumWorkers:  make(chan struct{}),
		queryHashesPerSec: make(chan float64),
		updateHashes:      make(chan uint64),
	}
}
