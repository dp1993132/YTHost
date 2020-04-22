package host

import (
	"context"
	"fmt"
	counter "github.com/yottachain/NodeOptimization/Counter"
	"log"
	"net/http"
	_ "net/http/pprof"
	"net/rpc"
	"os"
	"path"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/mr-tron/base58"
	"github.com/multiformats/go-multiaddr"
	mnet "github.com/multiformats/go-multiaddr-net"
	"github.com/yottachain/NodeOptimization"
	"github.com/yottachain/YTHost/client"
	"github.com/yottachain/YTHost/clientStore"
	"github.com/yottachain/YTHost/config"
	"github.com/yottachain/YTHost/connAutoCloser"
	"github.com/yottachain/YTHost/option"
	"github.com/yottachain/YTHost/peerInfo"
	"github.com/yottachain/YTHost/service"
)

//type Host interface {
//	Accept()
//	Addrs() []multiaddr.Multiaddr
//	Server() *rpc.Server
//	Config() *config.Config
//	Connect(ctx context.Context, pid peer.ID, mas []multiaddr.Multiaddr) (*client.YTHostClient, error)
//	RegisterHandler(id service.MsgId, handlerFunc service.Handler)
//}

type host struct {
	cfg      *config.Config
	listener mnet.Listener
	srv      *rpc.Server
	service.HandlerMap
	clientStore *clientStore.ClientStore
	optmizer    *optimizer.Optmizer
}

func NewHost(options ...option.Option) (*host, error) {
	hst := new(host)
	hst.optmizer = optimizer.New()

	// 计算得分
	hst.optmizer.GetScore = func(row counter.NodeCountRow) int64 {
		defer func() {
			err := recover()
			if err != nil {
				fmt.Println(err.(error).Error())
			}
		}()
		// 【成功,失败,延迟大于300,1000,3000毫秒】
		var w []int64 = []int64{50, -25, -5, -10, -15}
		var source int64 = 0

		for k, v := range w {
			source = source + row[k]*v
		}

		return source
	}
	go hst.optmizer.Run(context.Background())

	// 打印计数器
	go func() {
		for {
			<-time.After(time.Minute)
			logpath := path.Join(path.Dir(os.Args[0]), "opt.log")
			fl, err := os.OpenFile(logpath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
			if err != nil {
				continue
			}

			hst.optmizer.Lock()
			defer hst.optmizer.Unlock()
			for k, v := range hst.optmizer.NodeCountTable {
				fmt.Fprintf(fl, "%s,%d,%d,%d,%d,%d", k, v[0], v[1], v[2], v[3], v[4])
			}
		}
	}()

	hst.cfg = config.NewConfig()

	for _, bindOp := range options {
		bindOp(hst.cfg)
	}

	ls, err := mnet.Listen(hst.cfg.ListenAddr)

	if err != nil {
		return nil, err
	}

	hst.listener = ls

	srv := rpc.NewServer()
	hst.srv = srv

	hst.HandlerMap = make(service.HandlerMap)

	hst.clientStore = clientStore.NewClientStore(hst.Connect)

	if hst.cfg.PProf != "" {
		go func() {
			if err := http.ListenAndServe(hst.cfg.PProf, nil); err != nil {
				fmt.Println("PProf open fail:", err)
			} else {
				fmt.Println("PProf debug open:", hst.cfg.PProf)
			}
		}()
	}

	return hst, nil
}

func (hst *host) Accept() {
	addrService := new(service.AddrService)
	addrService.Info.ID = hst.cfg.ID
	addrService.Info.Addrs = hst.Addrs()
	addrService.PubKey = hst.Config().Privkey.GetPublic()

	msgService := new(service.MsgService)
	msgService.Handler = hst.HandlerMap
	msgService.Pi = peerInfo.PeerInfo{hst.cfg.ID, hst.Addrs()}

	if err := hst.srv.RegisterName("as", addrService); err != nil {
		panic(err)
	}

	if err := hst.srv.RegisterName("ms", msgService); err != nil {
		panic(err)
	}

	//for {
	//	hst.srv.Accept(mnet.NetListener(hst.listener))
	//}

	lis := mnet.NetListener(hst.listener)
	for {
		conn, err := lis.Accept()
		if err != nil {
			log.Print("rpc.Serve: accept:", err.Error())
			continue
		}
		ac := connAutoCloser.New(conn)
		ac.SetOuttime(time.Minute * 5)
		go hst.srv.ServeConn(ac)
	}
}

func (hst *host) Listenner() mnet.Listener {
	return hst.listener
}

func (hst *host) Server() *rpc.Server {
	return hst.srv
}

func (hst *host) Config() *config.Config {
	return hst.cfg
}

func (hst *host) ClientStore() *clientStore.ClientStore {
	return hst.clientStore
}

func (hst *host) Addrs() []multiaddr.Multiaddr {

	port, err := hst.listener.Multiaddr().ValueForProtocol(multiaddr.P_TCP)
	if err != nil {
		return nil
	}

	tcpMa, err := multiaddr.NewMultiaddr(fmt.Sprintf("/tcp/%s", port))

	if err != nil {
		return nil
	}

	var res []multiaddr.Multiaddr
	maddrs, err := mnet.InterfaceMultiaddrs()
	if err != nil {
		return nil
	}

	for _, ma := range maddrs {
		newMa := ma.Encapsulate(tcpMa)
		if mnet.IsIPLoopback(newMa) {
			continue
		}
		res = append(res, newMa)
	}
	return res
}

// Connect 连接远程节点
func (hst *host) Connect(ctx context.Context, pid peer.ID, mas []multiaddr.Multiaddr) (*client.YTHostClient, error) {

	conn, err := hst.connect(ctx, pid, mas)
	if err != nil {
		return nil, err
	}

	clt := rpc.NewClient(conn)
	ytclt, err := client.WarpClient(clt, &peer.AddrInfo{
		hst.cfg.ID,
		hst.Addrs(),
	}, hst.cfg.Privkey.GetPublic())
	if err != nil {
		return nil, err
	}
	return ytclt, nil
}

func (hst *host) connect(ctx context.Context, pid peer.ID, mas []multiaddr.Multiaddr) (mnet.Conn, error) {
	connChan := make(chan mnet.Conn)
	errChan := make(chan error)
	wg := sync.WaitGroup{}
	wg.Add(len(mas))

	go func() {
		wg.Wait()
		select {
		case errChan <- fmt.Errorf("dail all maddr fail"):
		case <-time.After(time.Millisecond * 500):
			return
		}
	}()

	for _, addr := range mas {
		go func(addr multiaddr.Multiaddr) {
			defer wg.Done()
			d := &mnet.Dialer{}
			if conn, err := d.DialContext(ctx, addr); err == nil {
				select {
				case connChan <- conn:
				case <-time.After(time.Second * 30):
				}
			} else {
				if hst.cfg.Debug {
					log.Println("conn error:", err)
				}
			}
		}(addr)
	}

	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("ctx quit")
		case conn := <-connChan:
			return conn, nil
		case err := <-errChan:
			return nil, err
		}
	}
}

// ConnectAddrStrings 连接字符串地址
func (hst *host) ConnectAddrStrings(ctx context.Context, id string, addrs []string) (*client.YTHostClient, error) {

	buf, _ := base58.Decode(id)
	pid, err := peer.IDFromBytes(buf)
	if err != nil {
		return nil, err
	}

	var mas = make([]multiaddr.Multiaddr, len(addrs))
	for k, v := range addrs {
		ma, err := multiaddr.NewMultiaddr(v)
		if err != nil {
			continue
		}
		mas[k] = ma
	}

	return hst.Connect(ctx, pid, mas)
}

// SendMsg 发送消息
func (hst *host) SendMsg(ctx context.Context, pid peer.ID, mid int32, msg []byte) ([]byte, error) {
	var status int
	st := time.Now()
	defer func() {
		//  标记成功失败
		hst.optmizer.Feedback(counter.InRow{pid.Pretty(), status})

		// 标记延迟
		if time.Now().Sub(st).Milliseconds() > 300 {
			hst.optmizer.Feedback(counter.InRow{pid.Pretty(), 2})
		} else if time.Now().Sub(st).Milliseconds() > 1000 {
			hst.optmizer.Feedback(counter.InRow{pid.Pretty(), 3})
		} else if time.Now().Sub(st).Milliseconds() > 3000 {
			hst.optmizer.Feedback(counter.InRow{pid.Pretty(), 4})
		}

		// 调用计次
		hst.optmizer.Feedback(counter.InRow{pid.Pretty(), 5})
	}()

	clt, ok := hst.ClientStore().GetClient(pid)
	if !ok {
		return nil, fmt.Errorf("no client ID is:%s", pid.Pretty())
	}

	res, err := clt.SendMsg(ctx, mid, msg)
	if err != nil {
		status = 1
	} else {
		status = 0
	}
	return res, err
}

func (hst *host) Optmizer() *optimizer.Optmizer {
	return hst.optmizer
}
