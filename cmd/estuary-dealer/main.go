package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/urfave/cli/v2"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"golang.org/x/net/websocket"
	"golang.org/x/xerrors"
	"gorm.io/gorm"

	"github.com/filecoin-project/lotus/api"
	lcli "github.com/filecoin-project/lotus/cli"
	"github.com/ipfs/go-blockservice"
	"github.com/ipfs/go-cid"
	blockstore "github.com/ipfs/go-ipfs-blockstore"
	ipld "github.com/ipfs/go-ipld-format"
	logging "github.com/ipfs/go-log"
	"github.com/ipfs/go-merkledag"
	"github.com/labstack/echo/v4"
	"github.com/whyrusleeping/estuary/drpc"
	"github.com/whyrusleeping/estuary/filclient"
	node "github.com/whyrusleeping/estuary/node"
	"github.com/whyrusleeping/estuary/pinner"
	"github.com/whyrusleeping/estuary/util"
	"github.com/whyrusleeping/memo"
)

var Tracer = otel.Tracer("dealer")

var log = logging.Logger("dealer")

func init() {
	if os.Getenv("FULLNODE_API_INFO") == "" {
		os.Setenv("FULLNODE_API_INFO", "wss://api.chain.love")
	}
}

func main() {
	logging.SetLogLevel("dt-impl", "debug")
	logging.SetLogLevel("dealer", "debug")
	logging.SetLogLevel("paych", "debug")
	logging.SetLogLevel("filclient", "debug")
	logging.SetLogLevel("dt_graphsync", "debug")
	logging.SetLogLevel("dt-chanmon", "debug")
	logging.SetLogLevel("markets", "debug")
	logging.SetLogLevel("data_transfer_network", "debug")
	logging.SetLogLevel("rpc", "info")
	logging.SetLogLevel("bs-wal", "info")

	app := cli.NewApp()
	app.Flags = []cli.Flag{
		&cli.StringFlag{
			Name:  "repo",
			Value: "~/.lotus",
		},
		&cli.StringFlag{
			Name:    "database",
			Value:   "sqlite=estuary-dealer.db",
			EnvVars: []string{"ESTUARY_DEALER_DATABASE"},
		},
		&cli.StringFlag{
			Name:    "apilisten",
			Usage:   "address for the api server to listen on",
			Value:   ":3005",
			EnvVars: []string{"ESTUARY_DEALER_API_LISTEN"},
		},
		&cli.StringFlag{
			Name:    "datadir",
			Usage:   "directory to store data in",
			Value:   ".",
			EnvVars: []string{"ESTUARY_DEALER_DATADIR"},
		},
		&cli.StringFlag{
			Name:  "estuary-api",
			Usage: "api endpoint for master estuary node",
			Value: "https://api.estuary.tech",
		},
		&cli.StringFlag{
			Name:     "auth-token",
			Usage:    "auth token for connecting to estuary",
			Required: true,
		},
		&cli.StringFlag{
			Name:     "handle",
			Usage:    "estuary dealer handle to use",
			Required: true,
		},
		&cli.StringFlag{
			Name:  "host",
			Usage: "url that this node is publicly dialable at",
		},
	}

	app.Action = func(cctx *cli.Context) error {
		ddir := cctx.String("datadir")

		cfg := &node.Config{
			ListenAddrs: []string{
				"/ip4/0.0.0.0/tcp/6745",
			},
			Blockstore:    filepath.Join(ddir, "blocks"),
			Libp2pKeyFile: filepath.Join(ddir, "peer.key"),
			Datastore:     filepath.Join(ddir, "leveldb"),
			WalletDir:     filepath.Join(ddir, "wallet"),
		}

		api, closer, err := lcli.GetGatewayAPI(cctx)
		if err != nil {
			return err
		}

		defer closer()

		nd, err := node.Setup(context.TODO(), cfg)
		if err != nil {
			return err
		}

		defaddr, err := nd.Wallet.GetDefault()
		if err != nil {
			return err
		}

		filc, err := filclient.NewClient(nd.Host, api, nd.Wallet, defaddr, nd.Blockstore, nd.Datastore, ddir)
		if err != nil {
			return err
		}

		db, err := setupDatabase(cctx.String("database"))
		if err != nil {
			return err
		}

		commpMemo := memo.NewMemoizer(func(ctx context.Context, k string) (interface{}, error) {
			c, err := cid.Decode(k)
			if err != nil {
				return nil, err
			}

			commpcid, size, err := filclient.GeneratePieceCommitment(ctx, c, nd.Blockstore)
			if err != nil {
				return nil, err
			}

			res := &commpResult{
				CommP: commpcid,
				Size:  size,
			}

			return res, nil
		})

		d := &Dealer{
			Node: nd,
			Api:  api,
			DB:   db,
			Filc: filc,

			commpMemo: commpMemo,

			outgoing: make(chan *drpc.Message),

			hostname:     "",
			estuaryHost:  cctx.String("estuary-api"),
			dealerHandle: cctx.String("handle"),
			dealerToken:  cctx.String("auth-token"),
		}
		d.PinMgr = pinner.NewPinManager(d.doPinning, d.onPinStatusUpdate)

		return d.ServeAPI(cctx.String("apilisten"))
	}

	if err := app.Run(os.Args); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

type Dealer struct {
	Node   *node.Node
	Api    api.Gateway
	DB     *gorm.DB
	PinMgr *pinner.PinManager
	Filc   *filclient.FilClient

	addPinLk sync.Mutex

	outgoing chan *drpc.Message

	hostname     string
	estuaryHost  string
	dealerHandle string
	dealerToken  string

	commpMemo *memo.Memoizer
}

func (d *Dealer) RunRpcConnection() error {
	for {
		conn, err := d.dialConn()
		if err != nil {
			log.Errorf("failed to dial estuary rpc endpoint: %s", err)
			time.Sleep(time.Second * 10)
			continue
		}

		if err := d.runRpc(conn); err != nil {
			log.Errorf("rpc routine exited with an error: %s", err)
			time.Sleep(time.Second * 10)
			continue
		}

		log.Warnf("rpc routine exited with no error, reconnecting...")
		time.Sleep(time.Second)
	}
}

func (d *Dealer) runRpc(conn *websocket.Conn) error {
	defer conn.Close()

	readDone := make(chan struct{})

	// Send hello message
	hello, err := d.getHelloMessage()
	if err != nil {
		return err
	}

	if err := websocket.JSON.Send(conn, hello); err != nil {
		return err
	}

	go func() {
		defer close(readDone)

		for {
			var cmd drpc.Command
			if err := websocket.JSON.Receive(conn, &cmd); err != nil {
				log.Errorf("failed to read command from websocket: %w", err)
				return
			}

			go func(cmd *drpc.Command) {
				if err := d.handleRpcCmd(cmd); err != nil {
					log.Errorf("failed to handle rpc command: %s", err)
				}
			}(&cmd)
		}
	}()

	for {
		select {
		case <-readDone:
			return fmt.Errorf("read routine exited, assuming socket is closed")
		case msg := <-d.outgoing:
			conn.SetWriteDeadline(time.Now().Add(time.Second * 30))
			if err := websocket.JSON.Send(conn, msg); err != nil {
				log.Errorf("failed to send message: %s", err)
			}
			conn.SetWriteDeadline(time.Time{})
		}
	}
}

func (d *Dealer) getHelloMessage() (*drpc.Hello, error) {
	return &drpc.Hello{
		Host:   d.hostname,
		PeerID: d.Node.Host.ID().Pretty(),
	}, nil
}

func (d *Dealer) dialConn() (*websocket.Conn, error) {
	cfg, err := websocket.NewConfig(d.estuaryHost+"/dealer/conn", "http://localhost")
	if err != nil {
		return nil, err
	}

	cfg.Header.Set("Authorization", "Bearer "+d.dealerToken)

	conn, err := websocket.DialConfig(cfg)
	if err != nil {
		return nil, err
	}

	return conn, nil
}

type User struct {
	ID       uint
	Username string
	Perms    int
}

func (d *Dealer) checkTokenAuth(token string) (*User, error) {
	req, err := http.NewRequest("GET", d.estuaryHost+"/viewer", nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		var herr util.HttpError
		if err := json.NewDecoder(resp.Body).Decode(&herr); err != nil {
			return nil, fmt.Errorf("authentication check returned unexpected error, code %d", resp.StatusCode)
		}

		return nil, fmt.Errorf("authentication check failed: %s(%d)", herr.Message, herr.Code)
	}

	var out util.ViewerResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}

	return &User{
		ID:       out.ID,
		Username: out.Username,
		Perms:    out.Perms,
	}, nil
}

func (d *Dealer) AuthRequired(level int) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			auth, err := util.ExtractAuth(c)
			if err != nil {
				return err
			}

			u, err := d.checkTokenAuth(auth)
			if err != nil {
				return err
			}

			if u.Perms >= level {
				c.Set("user", u)
				return next(c)
			}

			log.Warnw("User not authorized", "user", u.ID, "perms", u.Perms, "required", level)

			return &util.HttpError{
				Code:    401,
				Message: util.ERR_NOT_AUTHORIZED,
			}
		}
	}
}

func withUser(f func(echo.Context, *User) error) func(echo.Context) error {
	return func(c echo.Context) error {
		u, ok := c.Get("user").(*User)
		if !ok {
			return fmt.Errorf("endpoint not called with proper authentication")
		}

		return f(c, u)
	}
}

func (d *Dealer) ServeAPI(listen string) error {
	e := echo.New()

	content := e.Group("/content")
	content.Use(d.AuthRequired(util.PermLevelUser))
	content.POST("/add", withUser(d.handleAdd))
	//content.POST("/add-ipfs", withUser(d.handleAddIpfs))
	//content.POST("/add-car", withUser(d.handleAddCar))

	return e.Start(listen)
}

func (d *Dealer) handleAdd(e echo.Context, u *User) error {
	panic("nyi")
}

// TODO: mostly copy paste from estuary, dedup code
func (d *Dealer) doPinning(ctx context.Context, op *pinner.PinningOperation) error {
	ctx, span := Tracer.Start(ctx, "doPinning")
	defer span.End()

	for _, pi := range op.Peers {
		if err := d.Node.Host.Connect(ctx, pi); err != nil {
			log.Warnf("failed to connect to origin node for pinning operation: %s", err)
		}
	}

	bserv := blockservice.New(d.Node.Blockstore, d.Node.Bitswap)
	dserv := merkledag.NewDAGService(bserv)
	dsess := merkledag.NewSession(ctx, dserv)

	if err := d.addDatabaseTrackingToContent(ctx, op.ContId, dsess, d.Node.Blockstore, op.Obj); err != nil {
		return err
	}

	/*
		if op.Replace > 0 {
			if err := s.CM.RemoveContent(ctx, op.Replace, true); err != nil {
				log.Infof("failed to remove content in replacement: %d", op.Replace)
			}
		}
	*/

	// this provide call goes out immediately
	if err := d.Node.FullRT.Provide(ctx, op.Obj, true); err != nil {
		log.Infof("provider broadcast failed: %s", err)
	}

	// this one adds to a queue
	if err := d.Node.Provider.Provide(op.Obj); err != nil {
		log.Infof("providing failed: %s", err)
	}

	return nil
}

// TODO: mostly copy paste from estuary, dedup code
func (d *Dealer) addDatabaseTrackingToContent(ctx context.Context, pin uint, dserv ipld.NodeGetter, bs blockstore.Blockstore, root cid.Cid) error {
	ctx, span := Tracer.Start(ctx, "computeObjRefsUpdate")
	defer span.End()

	var dbpin Pin
	if err := d.DB.First(&dbpin, "id = ?", pin).Error; err != nil {
		return err
	}

	var objects []*Object
	var totalSize int64
	cset := cid.NewSet()

	err := merkledag.Walk(ctx, func(ctx context.Context, c cid.Cid) ([]*ipld.Link, error) {
		node, err := dserv.Get(ctx, c)
		if err != nil {
			return nil, err
		}

		objects = append(objects, &Object{
			Cid:  util.DbCID{c},
			Size: len(node.RawData()),
		})

		totalSize += int64(len(node.RawData()))

		if c.Type() == cid.Raw {
			return nil, nil
		}

		return node.Links(), nil
	}, root, cset.Visit, merkledag.Concurrent())
	if err != nil {
		return err
	}

	span.SetAttributes(
		attribute.Int64("totalSize", totalSize),
		attribute.Int("numObjects", len(objects)),
	)

	if err := d.DB.CreateInBatches(objects, 300).Error; err != nil {
		return xerrors.Errorf("failed to create objects in db: %w", err)
	}

	if err := d.DB.Model(Pin{}).Where("id = ?", pin).UpdateColumns(map[string]interface{}{
		"active":  true,
		"size":    totalSize,
		"pinning": false,
	}).Error; err != nil {
		return xerrors.Errorf("failed to update content in database: %w", err)
	}

	refs := make([]ObjRef, len(objects))
	for i := range refs {
		refs[i].Pin = pin
		refs[i].Object = objects[i].ID
	}

	if err := d.DB.CreateInBatches(refs, 500).Error; err != nil {
		return xerrors.Errorf("failed to create refs: %w", err)
	}

	d.sendPinCompleteMessage(ctx, dbpin.Content, totalSize, objects)

	return nil
}

func (d *Dealer) onPinStatusUpdate(cont uint, status string) {
	go func() {
		if err := d.sendRpcMessage(context.TODO(), &drpc.Message{
			Op: "UpdatePinStatus",
			Params: drpc.MsgParams{
				UpdatePinStatus: &drpc.UpdatePinStatus{
					DBID:   cont,
					Status: status,
				},
			},
		}); err != nil {
			log.Errorf("failed to send pin status update: %s", err)
		}
	}()
}