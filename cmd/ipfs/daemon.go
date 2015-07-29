package main

import (
	"bufio"
	_ "expvar"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"sort"
	"sync"

	_ "github.com/ipfs/go-ipfs/Godeps/_workspace/src/github.com/codahale/metrics/runtime"
	ma "github.com/ipfs/go-ipfs/Godeps/_workspace/src/github.com/jbenet/go-multiaddr"
	"github.com/ipfs/go-ipfs/Godeps/_workspace/src/github.com/jbenet/go-multiaddr-net"

	key "github.com/ipfs/go-ipfs/blocks/key"
	cmds "github.com/ipfs/go-ipfs/commands"
	"github.com/ipfs/go-ipfs/core"
	commands "github.com/ipfs/go-ipfs/core/commands"
	corehttp "github.com/ipfs/go-ipfs/core/corehttp"
	"github.com/ipfs/go-ipfs/core/corerouting"
	conn "github.com/ipfs/go-ipfs/p2p/net/conn"
	peer "github.com/ipfs/go-ipfs/p2p/peer"
	fsrepo "github.com/ipfs/go-ipfs/repo/fsrepo"
	util "github.com/ipfs/go-ipfs/util"
)

const (
	initOptionKwd             = "init"
	routingOptionKwd          = "routing"
	routingOptionSupernodeKwd = "supernode"
	mountKwd                  = "mount"
	writableKwd               = "writable"
	ipfsMountKwd              = "mount-ipfs"
	ipnsMountKwd              = "mount-ipns"
	unrestrictedApiAccessKwd  = "unrestricted-api"
	unencryptTransportKwd     = "disable-transport-encryption"
	// apiAddrKwd    = "address-api"
	// swarmAddrKwd  = "address-swarm"
)

var daemonCmd = &cmds.Command{
	Helptext: cmds.HelpText{
		Tagline: "Run a network-connected IPFS node",
		ShortDescription: `
'ipfs daemon' runs a persistent IPFS daemon that can serve commands
over the network. Most applications that use IPFS will do so by
communicating with a daemon over the HTTP API. While the daemon is
running, calls to 'ipfs' commands will be sent over the network to
the daemon.

The daemon will start listening on ports on the network, which are
documented in (and can be modified through) 'ipfs config Addresses'.
For example, to change the 'Gateway' port:

    ipfs config Addresses.Gateway /ip4/127.0.0.1/tcp/8082

The API address can be changed the same way:

   ipfs config Addresses.API /ip4/127.0.0.1/tcp/5002

Make sure to restart the daemon after changing addresses.

By default, the gateway is only accessible locally. To expose it to other computers
in the network, use 0.0.0.0 as the ip address:

   ipfs config Addresses.Gateway /ip4/0.0.0.0/tcp/8080

Be careful if you expose the API. It is a security risk, as anyone could control
your node remotely. If you need to control the node remotely, make sure to protect
the port as you would other services or database (firewall, authenticated proxy, etc).

In order to explicitly allow Cross-Origin requests, export the root url as
environment variable API_ORIGIN.  For example, to allow a local server at port 8888,
run this then restart the daemon:

   export API_ORIGIN="http://localhost:8888/`,
	},

	Options: []cmds.Option{
		cmds.BoolOption(initOptionKwd, "Initialize IPFS with default settings if not already initialized"),
		cmds.StringOption(routingOptionKwd, "Overrides the routing option (dht, supernode)"),
		cmds.BoolOption(mountKwd, "Mounts IPFS to the filesystem"),
		cmds.BoolOption(writableKwd, "Enable writing objects (with POST, PUT and DELETE)"),
		cmds.StringOption(ipfsMountKwd, "Path to the mountpoint for IPFS (if using --mount)"),
		cmds.StringOption(ipnsMountKwd, "Path to the mountpoint for IPNS (if using --mount)"),
		cmds.BoolOption(unrestrictedApiAccessKwd, "Allow API access to unlisted hashes"),
		cmds.BoolOption(unencryptTransportKwd, "Disable transport encryption (for debugging protocols)"),

		// TODO: add way to override addresses. tricky part: updating the config if also --init.
		// cmds.StringOption(apiAddrKwd, "Address for the daemon rpc API (overrides config)"),
		// cmds.StringOption(swarmAddrKwd, "Address for the swarm socket (overrides config)"),
	},
	Subcommands: map[string]*cmds.Command{},
	Run:         daemonFunc,
}

// defaultMux tells mux to serve path using the default muxer. This is
// mostly useful to hook up things that register in the default muxer,
// and don't provide a convenient http.Handler entry point, such as
// expvar and http/pprof.
func defaultMux(path string) func(node *core.IpfsNode, mux *http.ServeMux) (*http.ServeMux, error) {
	return func(node *core.IpfsNode, mux *http.ServeMux) (*http.ServeMux, error) {
		mux.Handle(path, http.DefaultServeMux)
		return mux, nil
	}
}

func daemonFunc(req cmds.Request, res cmds.Response) {
	// let the user know we're going.
	fmt.Printf("Initializing daemon...\n")

	ctx := req.InvocContext()

	go func() {
		select {
		case <-req.Context().Done():
			fmt.Println("Received interrupt signal, shutting down...")
		}
	}()

	// check transport encryption flag.
	unencrypted, _, _ := req.Option(unencryptTransportKwd).Bool()
	if unencrypted {
		log.Warningf(`Running with --%s: All connections are UNENCRYPTED.
		You will not be able to connect to regular encrypted networks.`, unencryptTransportKwd)
		conn.EncryptConnections = false
	}

	// first, whether user has provided the initialization flag. we may be
	// running in an uninitialized state.
	initialize, _, err := req.Option(initOptionKwd).Bool()
	if err != nil {
		res.SetError(err, cmds.ErrNormal)
		return
	}

	if initialize {

		// now, FileExists is our best method of detecting whether IPFS is
		// configured. Consider moving this into a config helper method
		// `IsInitialized` where the quality of the signal can be improved over
		// time, and many call-sites can benefit.
		if !util.FileExists(req.InvocContext().ConfigRoot) {
			err := initWithDefaults(os.Stdout, req.InvocContext().ConfigRoot)
			if err != nil {
				res.SetError(err, cmds.ErrNormal)
				return
			}
		}
	}

	// acquire the repo lock _before_ constructing a node. we need to make
	// sure we are permitted to access the resources (datastore, etc.)
	repo, err := fsrepo.Open(req.InvocContext().ConfigRoot)
	if err != nil {
		res.SetError(err, cmds.ErrNormal)
		return
	}

	cfg, err := ctx.GetConfig()
	if err != nil {
		res.SetError(err, cmds.ErrNormal)
		return
	}

	// Start assembling corebuilder
	nb := core.NewNodeBuilder().Online()
	nb.SetRepo(repo)

	routingOption, _, err := req.Option(routingOptionKwd).String()
	if err != nil {
		res.SetError(err, cmds.ErrNormal)
		return
	}
	if routingOption == routingOptionSupernodeKwd {
		servers, err := repo.Config().SupernodeRouting.ServerIPFSAddrs()
		if err != nil {
			res.SetError(err, cmds.ErrNormal)
			repo.Close() // because ownership hasn't been transferred to the node
			return
		}
		var infos []peer.PeerInfo
		for _, addr := range servers {
			infos = append(infos, peer.PeerInfo{
				ID:    addr.ID(),
				Addrs: []ma.Multiaddr{addr.Transport()},
			})
		}
		nb.SetRouting(corerouting.SupernodeClient(infos...))
	}

	node, err := nb.Build(req.Context())
	if err != nil {
		log.Error("error from node construction: ", err)
		res.SetError(err, cmds.ErrNormal)
		return
	}

	printSwarmAddrs(node)

	defer func() {
		// We wait for the node to close first, as the node has children
		// that it will wait for before closing, such as the API server.
		node.Close()

		select {
		case <-req.Context().Done():
			log.Info("Gracefully shut down daemon")
		default:
		}
	}()

	req.InvocContext().ConstructNode = func() (*core.IpfsNode, error) {
		return node, nil
	}

	// construct api endpoint - every time
	err, apiErrc := serveHTTPApi(req)
	if err != nil {
		res.SetError(err, cmds.ErrNormal)
		return
	}

	// construct http gateway - if it is set in the config
	var gwErrc <-chan error
	if len(cfg.Addresses.Gateway) > 0 {
		var err error
		err, gwErrc = serveHTTPGateway(req)
		if err != nil {
			res.SetError(err, cmds.ErrNormal)
			return
		}
	}

	// construct fuse mountpoints - if the user provided the --mount flag
	mount, _, err := req.Option(mountKwd).Bool()
	if err != nil {
		res.SetError(err, cmds.ErrNormal)
		return
	}
	if mount {
		if err := mountFuse(req); err != nil {
			res.SetError(err, cmds.ErrNormal)
			return
		}
	}

	// collect long-running errors and block for shutdown
	// TODO(cryptix): our fuse currently doesnt follow this pattern for graceful shutdown
	for err := range merge(apiErrc, gwErrc) {
		if err != nil {
			res.SetError(err, cmds.ErrNormal)
			return
		}
	}
}

// serveHTTPApi collects options, creates listener, prints status message and starts serving requests
func serveHTTPApi(req cmds.Request) (error, <-chan error) {
	cfg, err := req.InvocContext().GetConfig()
	if err != nil {
		return fmt.Errorf("serveHTTPApi: GetConfig() failed: %s", err), nil
	}

	apiMaddr, err := ma.NewMultiaddr(cfg.Addresses.API)
	if err != nil {
		return fmt.Errorf("serveHTTPApi: invalid API address: %q (err: %s)", cfg.Addresses.API, err), nil
	}

	apiLis, err := manet.Listen(apiMaddr)
	if err != nil {
		return fmt.Errorf("serveHTTPApi: manet.Listen(%s) failed: %s", apiMaddr, err), nil
	}
	// we might have listened to /tcp/0 - lets see what we are listing on
	apiMaddr = apiLis.Multiaddr()
	fmt.Printf("API server listening on %s\n", apiMaddr)

	unrestricted, _, err := req.Option(unrestrictedApiAccessKwd).Bool()
	if err != nil {
		return fmt.Errorf("serveHTTPApi: Option(%s) failed: %s", unrestrictedApiAccessKwd, err), nil
	}

	var whitelist key.KeySet
	if !unrestricted {
		whitelist = key.Threadsafe(key.NewKeySet())
		for _, webuipath := range corehttp.WebUIPaths {
			// extract the key
			whitelist.Add(key.B58KeyDecode(webuipath[6:]))
		}
	}

	apiGw := corehttp.NewGateway(corehttp.GatewayConfig{
		Writable:  true,
		WhiteList: whitelist,
	})

	var opts = []corehttp.ServeOption{
		corehttp.CommandsOption(*req.InvocContext()),
		corehttp.WebUIOption,
		apiGw.ServeOption(),
		corehttp.VersionOption(),
		defaultMux("/debug/vars"),
		defaultMux("/debug/pprof/"),
		corehttp.LogOption(),
		corehttp.PrometheusOption("/debug/metrics/prometheus"),
	}

	if len(cfg.Gateway.RootRedirect) > 0 {
		opts = append(opts, corehttp.RedirectOption("", cfg.Gateway.RootRedirect))
	}

	node, err := req.InvocContext().ConstructNode()
	if err != nil {
		return fmt.Errorf("serveHTTPGateway: ConstructNode() failed: %s", err), nil
	}

	errc := make(chan error)
	go func() {
		errc <- corehttp.Serve(node, apiLis.NetListener(), opts...)
		close(errc)
	}()
	return nil, errc
}

// printSwarmAddrs prints the addresses of the host
func printSwarmAddrs(node *core.IpfsNode) {
	var addrs []string
	for _, addr := range node.PeerHost.Addrs() {
		addrs = append(addrs, addr.String())
	}
	sort.Sort(sort.StringSlice(addrs))

	for _, addr := range addrs {
		fmt.Printf("Swarm listening on %s\n", addr)
	}
}

// serveHTTPGateway collects options, creates listener, prints status message and starts serving requests
func serveHTTPGateway(req cmds.Request) (error, <-chan error) {
	cfg, err := req.InvocContext().GetConfig()
	if err != nil {
		return fmt.Errorf("serveHTTPGateway: GetConfig() failed: %s", err), nil
	}

	gatewayMaddr, err := ma.NewMultiaddr(cfg.Addresses.Gateway)
	if err != nil {
		return fmt.Errorf("serveHTTPGateway: invalid gateway address: %q (err: %s)", cfg.Addresses.Gateway, err), nil
	}

	writable, writableOptionFound, err := req.Option(writableKwd).Bool()
	if err != nil {
		return fmt.Errorf("serveHTTPGateway: req.Option(%s) failed: %s", writableKwd, err), nil
	}
	if !writableOptionFound {
		writable = cfg.Gateway.Writable
	}

	gwLis, err := manet.Listen(gatewayMaddr)
	if err != nil {
		return fmt.Errorf("serveHTTPGateway: manet.Listen(%s) failed: %s", gatewayMaddr, err), nil
	}
	// we might have listened to /tcp/0 - lets see what we are listing on
	gatewayMaddr = gwLis.Multiaddr()

	if writable {
		fmt.Printf("Gateway (writable) server listening on %s\n", gatewayMaddr)
	} else {
		fmt.Printf("Gateway (readonly) server listening on %s\n", gatewayMaddr)
	}

	var blacklist key.KeySet
	var whitelist key.KeySet
	if cfg.Gateway.BlackList != "" {
		l, err := loadKeySetFromURL(cfg.Gateway.BlackList)
		if err != nil {
			return err, nil
		}
		blacklist = l
	}

	if cfg.Gateway.WhiteList != "" {
		l, err := loadKeySetFromURL(cfg.Gateway.WhiteList)
		if err != nil {
			return err, nil
		}
		whitelist = l
	}

	gateway := corehttp.Gateway{
		Config: corehttp.GatewayConfig{
			BlackList: blacklist,
			WhiteList: whitelist,
		},
	}

	var opts = []corehttp.ServeOption{
		corehttp.VersionOption(),
		corehttp.IPNSHostnameOption(),
		gateway.ServeOption(),
	}

	if len(cfg.Gateway.RootRedirect) > 0 {
		opts = append(opts, corehttp.RedirectOption("", cfg.Gateway.RootRedirect))
	}

	node, err := req.InvocContext().ConstructNode()
	if err != nil {
		return fmt.Errorf("serveHTTPGateway: ConstructNode() failed: %s", err), nil
	}

	errc := make(chan error)
	go func() {
		errc <- corehttp.Serve(node, gwLis.NetListener(), opts...)
		close(errc)
	}()
	return nil, errc
}

func loadKeySetFromURL(url string) (key.KeySet, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}

	ks := key.NewKeySet()
	scan := bufio.NewScanner(resp.Body)
	for scan.Scan() {
		k := key.B58KeyDecode(scan.Text())
		if k == "" {
			return nil, fmt.Errorf("invalid key in set")
		}
		ks.Add(k)
	}
	return key.Threadsafe(ks), nil
}

//collects options and opens the fuse mountpoint
func mountFuse(req cmds.Request) error {
	cfg, err := req.InvocContext().GetConfig()
	if err != nil {
		return fmt.Errorf("mountFuse: GetConfig() failed: %s", err)
	}

	fsdir, found, err := req.Option(ipfsMountKwd).String()
	if err != nil {
		return fmt.Errorf("mountFuse: req.Option(%s) failed: %s", ipfsMountKwd, err)
	}
	if !found {
		fsdir = cfg.Mounts.IPFS
	}

	nsdir, found, err := req.Option(ipnsMountKwd).String()
	if err != nil {
		return fmt.Errorf("mountFuse: req.Option(%s) failed: %s", ipnsMountKwd, err)
	}
	if !found {
		nsdir = cfg.Mounts.IPNS
	}

	node, err := req.InvocContext().ConstructNode()
	if err != nil {
		return fmt.Errorf("mountFuse: ConstructNode() failed: %s", err)
	}

	err = commands.Mount(node, fsdir, nsdir)
	if err != nil {
		return err
	}
	fmt.Printf("IPFS mounted at: %s\n", fsdir)
	fmt.Printf("IPNS mounted at: %s\n", nsdir)
	return nil
}

// merge does fan-in of multiple read-only error channels
// taken from http://blog.golang.org/pipelines
func merge(cs ...<-chan error) <-chan error {
	var wg sync.WaitGroup
	out := make(chan error)

	// Start an output goroutine for each input channel in cs.  output
	// copies values from c to out until c is closed, then calls wg.Done.
	output := func(c <-chan error) {
		for n := range c {
			out <- n
		}
		wg.Done()
	}
	for _, c := range cs {
		if c != nil {
			wg.Add(1)
			go output(c)
		}
	}

	// Start a goroutine to close out once all the output goroutines are
	// done.  This must start after the wg.Add call.
	go func() {
		wg.Wait()
		close(out)
	}()
	return out
}
