package main

import (
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"golang.org/x/net/websocket" // 官方子包
)

// js版本原文：https://github.com/lhartikk/naivechain
// 解析：https://medium.com/@lhartikk/a-blockchain-in-200-lines-of-code-963cc1cc0e54#.dttbm9afr5

/////////////初始化、数据结构类型定义、错误处理函数等等/////////////

// TODO:?
const (
	queryLatest = iota
	queryAll
	responseBlockchain
)

// 区块类型声明
type Block struct {
	Index        int64  `json:"index"` // 索引
	PreviousHash string `json:"previousHash"` // 前置区块
	Timestamp    int64  `json:"timestamp"` // 时间戳
	Data         string `json:"data"` // 数据
	Hash         string `json:"hash"` // 哈希
}

// 起始区块指针
var genesisBlock = &Block{
	Index:        0, // 初始索引0
	PreviousHash: "0", // 初始区块的前置hash为"0"
	Timestamp:    1465154705,
	Data:         "my genesis block!!",
	Hash:         "816534932c2b7154836da6afc367695e6337db8a921823784c14378abed4f7d7", // TODO:同样算法计算出的？
}

// 全局变量声明与初始化
var (
	sockets      []*websocket.Conn // 存放ws连接指针的slice
	blockchain   = []*Block{genesisBlock} // 本地存放区块指针的slice，初始化时存入起始块。只使用内存来存储区块链。这意味着当节点终止时，数据不会被持久化。
	httpAddr     = flag.String("api", ":3001", "api server address.") // flag接收参数api，默认值:3301，含义是"api..."
	p2pAddr      = flag.String("p2p", ":6001", "p2p server address.") // 交易是无需信任中介参与的P2P（Peer-to-peer）交易。
	initialPeers = flag.String("peers", "ws://localhost:6001", "initial peers") // 初始peer的设置
)

// 为区块指针定义序列化区块内容的方法
func (b *Block) String() string {
	return fmt.Sprintf("index: %d,previousHash:%s,timestamp:%d,data:%s,hash:%s", b.Index, b.PreviousHash, b.Timestamp, b.Data, b.Hash)
}

// 为存放区块指针的slice声明类型别名
type ByIndex []*Block
// 为区块指针slice添加方法：计数、交换、比较序号大小
func (b ByIndex) Len() int           { return len(b) }
func (b ByIndex) Swap(i, j int)      { b[i], b[j] = b[j], b[i] }
func (b ByIndex) Less(i, j int) bool { return b[i].Index < b[j].Index }

// 服务器响应消息的数据结构
type ResponseBlockchain struct {
	Type int    `json:"type"`
	Data string `json:"data"`
}

// 统一的错误处理函数
func errFatal(msg string, err error) {
	if err != nil {
		log.Fatalln(msg, err)
	}
}

/////////////p2p连接、http响应等处理/////////////

// ws连接到1个或多个peer（本地启动一个peer时，需要指定连接到其他1个或多个peer）
func connectToPeers(peersAddr []string) {
	for _, peer := range peersAddr {
		if peer == "" {
			continue
		}
		// 连接peer
		ws, err := websocket.Dial(peer, "", peer)
		if err != nil {
			log.Println("dial to peer", err)
			continue
		}
		initConnection(ws)
	}
}

// 初始化ws连接
func initConnection(ws *websocket.Conn) {
	// 与其他节点通信：
	// 节点的一个基本部分是与其他节点共享和同步区块链。以下规则用于保持网络同步：
	// 当一个节点生成一个新的块时，它将它广播到网络中
	// 当一个节点连接到一个新的对等点peer时，它查询新peer最后的块
	// 当节点遇到一个索引大于当前已知块的块时，它要么将该块添加到当前链中，要么查询完整的区块链。
	go wsHandleP2P(ws) // 处理p2p连接（从其他peer同步区块，协程，循环监听其他peer返回的消息）

	log.Println("query latest block.")
	ws.Write(queryLatestMsg()) // 获取peer的最后一个区块
}

// 处理http请求：获取序列化的本地chain
func handleBlocks(w http.ResponseWriter, r *http.Request) {
	bs, _ := json.Marshal(blockchain)
	w.Write(bs)
}
// 处理http请求：提交数据，添加区块，并广播我添加的区块信息
func handleMineBlock(w http.ResponseWriter, r *http.Request) {
	var v struct {
		Data string `json:"data"`
	}
	decoder := json.NewDecoder(r.Body)
	defer r.Body.Close()
	err := decoder.Decode(&v)
	if err != nil {
		w.WriteHeader(http.StatusGone)
		log.Println("[API] invalid block data : ", err.Error())
		w.Write([]byte("invalid block data. " + err.Error() + "\n"))
		return
	}
	block := generateNextBlock(v.Data) // 生成新区块
	addBlock(block) // 添加新区块到本地存储
	broadcast(responseLatestMsg()) // 广播最后一个区块
}
// 处理http请求：获取peers信息，通过检查ws连接状态
func handlePeers(w http.ResponseWriter, r *http.Request) {
	var slice []string
	for _, socket := range sockets {
		if socket.IsClientConn() {
			slice = append(slice, strings.Replace(socket.LocalAddr().String(), "ws://", "", 1))
		} else {
			slice = append(slice, socket.Request().RemoteAddr)
		}
	}
	bs, _ := json.Marshal(slice)
	w.Write(bs)
}
// 处理http请求：添加peer
func handleAddPeer(w http.ResponseWriter, r *http.Request) {
	var v struct {
		Peer string `json:"peer"`
	}
	decoder := json.NewDecoder(r.Body)
	defer r.Body.Close()
	err := decoder.Decode(&v)
	if err != nil {
		w.WriteHeader(http.StatusGone)
		log.Println("[API] invalid peer data : ", err.Error())
		w.Write([]byte("invalid peer data. " + err.Error()))
		return
	}
	connectToPeers([]string{v.Peer})
}

// 处理ws连接（从其他peer同步区块）
func wsHandleP2P(ws *websocket.Conn) {
	var (
		v    = &ResponseBlockchain{} // 服务器响应消息的指针
		peer = ws.LocalAddr().String() // LocalAddr返回客户端连接的WebSocket Origin，或者服务器端的WebSocket位置。
	)
	sockets = append(sockets, ws) // 将连接的ws地址加入ws指针slice(peers pool)

	for {
		var msg []byte
		err := websocket.Message.Receive(ws, &msg) // Message是一个编解码器，用于在WebSocket连接的帧中发送/接收文本/二进制数据。要发送/接收文本帧，使用字符串类型。要发送/接收二进制帧，使用[]字节类型。
		if err == io.EOF { // ws服务器发生IO错误，比如连接关闭
			log.Printf("p2p Peer[%s] shutdown, remove it form peers pool.\n", peer)
			break
		}
		if err != nil { // ws服务器其他错误
			log.Println("Can't receive p2p msg from ", peer, err.Error())
			break
		}

		log.Printf("Received[from %s]: %s.\n", peer, msg)
		err = json.Unmarshal(msg, v)
		errFatal("invalid p2p msg", err)

		// 判断消息体的类型
		switch v.Type {
		case queryLatest:
			// 响应其他peer查询最后区块的消息
			v.Type = responseBlockchain

			bs := responseLatestMsg()
			log.Printf("responseLatestMsg: %s\n", bs)
			ws.Write(bs)

		case queryAll:
			// 响应其他peer查询所有区块的消息
			d, _ := json.Marshal(blockchain)
			v.Type = responseBlockchain
			v.Data = string(d)
			bs, _ := json.Marshal(v)
			log.Printf("responseChainMsg: %s\n", bs)
			ws.Write(bs)

		case responseBlockchain:
			// 处理其他peer返回的消息（向其他peer发起查询最后和全部的请求，收到其他peer的返回）
			handleBlockchainResponse([]byte(v.Data))
		}

	}
}
// 获取本地最后一个区块
func getLatestBlock() (block *Block) { return blockchain[len(blockchain)-1] }
// 响应最后的区块消息
func responseLatestMsg() (bs []byte) {
	var v = &ResponseBlockchain{Type: responseBlockchain}
	d, _ := json.Marshal(blockchain[len(blockchain)-1:]) // 取区块链指针slice的最后一项, TODO: 不是指针，是*Block类型
	v.Data = string(d)
	bs, _ = json.Marshal(v)
	return
}
// 请求peer的最后一个区块
func queryLatestMsg() []byte { return []byte(fmt.Sprintf("{\"type\": %d}", queryLatest)) }
func queryAllMsg() []byte    { return []byte(fmt.Sprintf("{\"type\": %d}", queryAll)) }
// 计算块hash
func calculateHashForBlock(b *Block) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%d%s%d%s", b.Index, b.PreviousHash, b.Timestamp, b.Data))))
}
// 生成新区块
func generateNextBlock(data string) (nb *Block) {
	var previousBlock = getLatestBlock()
	nb = &Block{
		Data:         data,
		PreviousHash: previousBlock.Hash,
		Index:        previousBlock.Index + 1,
		Timestamp:    time.Now().Unix(),
	}
	nb.Hash = calculateHashForBlock(nb)
	return
}
// 添加新区块到本地存储
func addBlock(b *Block) {
	if isValidNewBlock(b, getLatestBlock()) {
		blockchain = append(blockchain, b)
	}
}
// 判断chain中首位后面的都是有效块：块hash判等，前后两块的序号递增和hash关联
func isValidNewBlock(nb, pb *Block) (ok bool) {
	if nb.Hash == calculateHashForBlock(nb) &&
		pb.Index+1 == nb.Index &&
		pb.Hash == nb.PreviousHash {
		ok = true
	}
	return
}
// 判断要替换本地存储的chain（ws收到）是否合法：首位是起始块，后面都是有效块
func isValidChain(bc []*Block) bool {
	if bc[0].String() != genesisBlock.String() {
		log.Println("No same GenesisBlock.", bc[0].String())
		return false
	}
	var temp = []*Block{bc[0]}
	for i := 1; i < len(bc); i++ {
		if isValidNewBlock(bc[i], temp[i-1]) {
			temp = append(temp, bc[i])
		} else {
			return false
		}
	}
	return true
}
// 产生冲突时，用更长的串替换较短的串（向后确认），并将最后的block广播出去
// There should always be only one explicit set of blocks in the chain at a given time. In case of conflicts (e.g. two nodes both generate block number 72) we choose the chain that has the longest number of blocks.
func replaceChain(bc []*Block) {
	if isValidChain(bc) && len(bc) > len(blockchain) {
		log.Println("Received blockchain is valid. Replacing current blockchain with received blockchain.")
		blockchain = bc
		broadcast(responseLatestMsg()) // 向所有peers发送最后一个区块，判断是否已将本地与其他peers的chain正确同步（只看最后一块）
	} else {
		log.Println("Received blockchain invalid.")
	}
}
// 广播
func broadcast(msg []byte) {
	for n, socket := range sockets {
		_, err := socket.Write(msg)
		if err != nil {
			log.Printf("peer [%s] disconnected.", socket.RemoteAddr().String())
			sockets = append(sockets[0:n], sockets[n+1:]...)
		}
	}
}

// 处理从peer收到的区块链响应信息(一个或全部区块)
func handleBlockchainResponse(msg []byte) {
	var receivedBlocks = []*Block{}

	err := json.Unmarshal(msg, &receivedBlocks) // 解码
	errFatal("invalid blockchain", err)

	sort.Sort(ByIndex(receivedBlocks)) // 为区块slice排序

	latestBlockReceived := receivedBlocks[len(receivedBlocks)-1]
	latestBlockHeld := getLatestBlock()
	// 收到的最后一个区块序号 > 本地存放的最后一个区块序号
	if latestBlockReceived.Index > latestBlockHeld.Index {
		log.Printf("blockchain possibly behind. We got: %d Peer got: %d", latestBlockHeld.Index, latestBlockReceived.Index)
		if latestBlockHeld.Hash == latestBlockReceived.PreviousHash {
			// 本地存储的hash是peer持有的前一个hash，将peer持有的最后hash加到本地存储中
			log.Println("We can append the received block to our chain.")
			blockchain = append(blockchain, latestBlockReceived)
		} else if len(receivedBlocks) == 1 {
			// 如果只获取了peer的最后区块，却并不是紧跟在本地最后区块之后，就要从所有peers获取整个chain，继续处理所有peers对queryAll的Response(走下面else)
			log.Println("We have to query the chain from our peer.")
			broadcast(queryAllMsg())
		} else {
			// 收到的chain比本地长得多，需要将本地存储替换为收到的chain，并广播出去??
			log.Println("Received blockchain is longer than current blockchain.")
			replaceChain(receivedBlocks)
		}
	} else {
		log.Println("received blockchain is not longer than current blockchain. Do nothing.")
	}
}

// 主函数
func main() {
	flag.Parse() // 解析命令行参数
	connectToPeers(strings.Split(*initialPeers, ","))

	http.HandleFunc("/blocks", handleBlocks) // 获取本地区块链所有区块数据
	http.HandleFunc("/mine_block", handleMineBlock) // 支持添加数据到本地区块链的最后区块，并广播给所有peers
	http.HandleFunc("/peers", handlePeers) // 获取所有活跃的peer节点(ip:port)
	http.HandleFunc("/add_peer", handleAddPeer) // 添加一个peer，并将当前peer连接到新peer
	go func() {
		log.Println("Listen HTTP on", *httpAddr)
		errFatal("start api server", http.ListenAndServe(*httpAddr, nil))
	}()

	http.Handle("/", websocket.Handler(wsHandleP2P))
	log.Println("Listen P2P on ", *p2pAddr)
	errFatal("start p2p server", http.ListenAndServe(*p2pAddr, nil))
}
