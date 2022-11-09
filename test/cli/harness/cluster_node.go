package harness

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	logging "github.com/ipfs/go-log/v2"
	"github.com/ipfs/kubo/config"
	serial "github.com/ipfs/kubo/config/serialize"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
)

var log = logging.Logger("testharness")

type Node struct {
	ID  int
	Dir string

	APIListenAddr multiaddr.Multiaddr
	SwarmAddr     multiaddr.Multiaddr
	EnableMDNS    bool

	IPFSBin string
	Runner  *Runner

	daemon *RunResult
}

func BuildNode(ipfsBin, baseDir string, id int) Node {
	dir := filepath.Join(baseDir, strconv.Itoa(id))
	if err := os.MkdirAll(dir, 0755); err != nil {
		panic(err)
	}

	env := environToMap(os.Environ())
	env["IPFS_PATH"] = dir

	return Node{
		ID:      id,
		Dir:     dir,
		IPFSBin: ipfsBin,
		Runner: &Runner{
			Env: env,
			Dir: dir,
		},
	}
}

func (n *Node) ReadConfig() *config.Config {
	cfg, err := serial.Load(filepath.Join(n.Dir, "config"))
	if err != nil {
		panic(err)
	}
	return cfg
}

func (n *Node) WriteConfig(c *config.Config) {
	err := serial.WriteConfigFile(filepath.Join(n.Dir, "config"), c)
	if err != nil {
		panic(err)
	}
}

func (n *Node) MustRunIPFS(args ...string) RunResult {
	res := n.RunIPFS(args...)
	n.Runner.AssertNoError(res)
	return res
}

func (n *Node) RunIPFS(args ...string) RunResult {
	return n.Runner.Run(RunRequest{
		Path: n.IPFSBin,
		Args: args,
	})
}

// Init initializes and configures the IPFS node, after which it is ready to run.
func (n *Node) Init(ipfsArgs ...string) {
	// 	// h.MustRunIPFS("init", "--profile=test")
	// 	// h.Mkdirs("mountdir", "ipfs", "ipns")
	// 	// h.SetIPFSConfig("Mounts.IPFS", h.IPFSMountpoint)
	// 	// h.SetIPFSConfig("Mounts.IPNS", h.IPNSMountpoint)

	// 	// configPath := filepath.Join(h.IPFSPath, "config")
	// 	// b, err := ioutil.ReadFile(configPath)
	// 	// if err != nil {
	// 	// 	log.Fatalf("reading config file from %s: %s", configPath, err)
	// 	// }
	// 	// if len(b) == 0 {
	// 	// 	log.Panicf("expected non-empty config at %s", configPath)
	// 	// }

	n.Runner.MustRun(RunRequest{
		Path: n.IPFSBin,
		Args: append([]string{"init"}, ipfsArgs...),
	})

	if n.SwarmAddr == nil {
		swarmAddr, err := multiaddr.NewMultiaddr("/ip4/127.0.0.1/tcp/0")
		if err != nil {
			panic(err)
		}
		n.SwarmAddr = swarmAddr
	}

	if n.APIListenAddr == nil {
		apiAddr, err := multiaddr.NewMultiaddr("/ip4/127.0.0.1/tcp/0")
		if err != nil {
			panic(err)
		}
		n.APIListenAddr = apiAddr
	}

	cfg := n.ReadConfig()
	cfg.Bootstrap = []string{}
	cfg.Bootstrap = []string{}
	cfg.Addresses.Swarm = []string{n.SwarmAddr.String()}
	cfg.Addresses.API = []string{n.APIListenAddr.String()}
	cfg.Addresses.Gateway = []string{""}
	cfg.Discovery.MDNS.Enabled = n.EnableMDNS

	n.WriteConfig(cfg)
}

func (n *Node) Start(ipfsArgs ...string) {
	alive := n.IsAlive()
	if alive {
		log.Panicf("node %d is already running", n.ID)
	}

	daemonArgs := append([]string{"daemon"}, ipfsArgs...)
	log.Debugf("starting node %d", n.ID)
	res := n.Runner.MustRun(RunRequest{
		Path:    n.IPFSBin,
		Args:    daemonArgs,
		RunFunc: (*exec.Cmd).Start,
	})

	n.daemon = &res

	log.Debugf("node %d started, checking API", n.ID)
	n.WaitOnAPI()
}

func (n *Node) signalAndWait(watch <-chan struct{}, signal os.Signal, t time.Duration) bool {
	err := n.daemon.Cmd.Process.Signal(signal)
	if err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			log.Debugf("process for node %d has already finished", n.ID)
			return true
		}
		log.Panicf("error killing daemon for node %d with peer ID %s: %s", n.ID, n.PeerID(), err.Error())
	}
	timer := time.NewTimer(t)
	defer timer.Stop()
	select {
	case <-watch:
		return true
	case <-timer.C:
		return false
	}
}

func (n *Node) Stop() {
	log.Debugf("stopping node %d", n.ID)
	if n.daemon == nil {
		log.Debugf("didn't stop node %d since no daemon present", n.ID)
		return
	}
	watch := make(chan struct{}, 1)
	go func() {
		n.daemon.Cmd.Process.Wait()
		watch <- struct{}{}
	}()
	log.Debugf("signaling node %d with SIGTERM", n.ID)
	if n.signalAndWait(watch, syscall.SIGTERM, 1*time.Second) {
		return
	}
	log.Debugf("signaling node %d with SIGTERM", n.ID)
	if n.signalAndWait(watch, syscall.SIGTERM, 2*time.Second) {
		return
	}
	log.Debugf("signaling node %d with SIGQUIT", n.ID)
	if n.signalAndWait(watch, syscall.SIGQUIT, 5*time.Second) {
		return
	}
	log.Debugf("signaling node %d with SIGKILL", n.ID)
	if n.signalAndWait(watch, syscall.SIGKILL, 5*time.Second) {
		return
	}
	log.Panicf("timed out stopping node %d with peer ID %s", n.ID, n.PeerID())
}

func (n *Node) APIAddr() multiaddr.Multiaddr {
	ma, err := n.TryAPIAddr()
	if err != nil {
		panic(err)
	}
	return ma
}

func (n *Node) TryAPIAddr() (multiaddr.Multiaddr, error) {
	b, err := os.ReadFile(filepath.Join(n.Dir, "api"))
	if err != nil {
		return nil, err
	}
	ma, err := multiaddr.NewMultiaddr(string(b))
	if err != nil {
		return nil, err
	}
	return ma, nil
}

func (n *Node) checkAPI() bool {
	apiAddr, err := n.TryAPIAddr()
	if err != nil {
		log.Debugf("node %d API addr not available yet: %s", n.ID, err.Error())
		return false
	}
	ip, err := apiAddr.ValueForProtocol(multiaddr.P_IP4)
	if err != nil {
		panic(err)
	}
	port, err := apiAddr.ValueForProtocol(multiaddr.P_TCP)
	if err != nil {
		panic(err)
	}
	url := fmt.Sprintf("http://%s:%s/api/v0/id", ip, port)
	log.Debugf("checking API for node %d at %s", n.ID, url)
	httpResp, err := http.Post(url, "", nil)
	if err != nil {
		log.Debugf("node %d API check error: %s", err.Error())
		return false
	}
	defer httpResp.Body.Close()
	resp := struct {
		ID string
	}{}

	respBytes, err := io.ReadAll(httpResp.Body)
	if err != nil {
		log.Debugf("error reading API check response for node %d: %s", n.ID, err.Error())
		return false
	}
	log.Debugf("got API check response for node %d: %s", n.ID, string(respBytes))

	err = json.Unmarshal(respBytes, &resp)
	if err != nil {
		log.Debugf("error decoding API check response for node %d: %s", n.ID, err.Error())
		return false
	}
	if resp.ID == "" {
		log.Debugf("API check response for node %d did not contain a Peer ID", n.ID)
		return false
	}
	respPeerID, err := peer.Decode(resp.ID)
	if err != nil {
		panic(err)
	}

	peerID := n.PeerID()
	if respPeerID != peerID {
		log.Panicf("expected peer ID %s but got %s", peerID, resp.ID)
	}

	log.Debugf("API check for node %d successful", n.ID)
	return true
}

func (n *Node) PeerID() peer.ID {
	cfg := n.ReadConfig()
	id, err := peer.Decode(cfg.Identity.PeerID)
	if err != nil {
		panic(err)
	}
	return id
}

func (n *Node) WaitOnAPI() {
	log.Debugf("waiting on API for node %d", n.ID)
	for i := 0; i < 50; i++ {
		if n.checkAPI() {
			return
		}
		time.Sleep(400 * time.Millisecond)
	}
	log.Panicf("node %d with peer ID %s failed to come online: \n%s\n\n%s", n.ID, n.PeerID(), n.daemon.Stderr.String(), n.daemon.Stdout.String())
}

func (n *Node) IsAlive() bool {
	if n.daemon == nil || n.daemon.Cmd == nil || n.daemon.Cmd.Process == nil {
		return false
	}
	log.Debugf("signaling node %d daemon process for liveness check", n.ID)
	err := n.daemon.Cmd.Process.Signal(syscall.Signal(0))
	if err == nil {
		log.Debugf("node %d daemon is alive", n.ID)
		return true
	}
	log.Debugf("node %d daemon not alive: %s", err.Error())
	return false
}

func (n *Node) SwarmAddrs() []multiaddr.Multiaddr {
	res := n.Runner.MustRun(RunRequest{
		Path: n.IPFSBin,
		Args: []string{"swarm", "addrs", "local"},
	})
	ipfsProtocol := multiaddr.ProtocolWithCode(multiaddr.P_IPFS).Name
	peerID := n.PeerID()
	out := strings.TrimSpace(res.Stdout.String())
	outLines := strings.Split(out, "\n")
	var addrs []multiaddr.Multiaddr
	for _, addrStr := range outLines {
		ma, err := multiaddr.NewMultiaddr(addrStr)
		if err != nil {
			panic(err)
		}

		// add the peer ID to the multiaddr if it doesn't have it
		_, err = ma.ValueForProtocol(multiaddr.P_IPFS)
		if errors.Is(err, multiaddr.ErrProtocolNotFound) {
			comp, err := multiaddr.NewComponent(ipfsProtocol, peerID.String())
			if err != nil {
				panic(err)
			}
			ma = ma.Encapsulate(comp)
		}
		addrs = append(addrs, ma)
	}
	return addrs
}

func (n *Node) Connect(other *Node) {
	n.Runner.MustRun(RunRequest{
		Path: n.IPFSBin,
		Args: []string{"swarm", "connect", other.SwarmAddrs()[0].String()},
	})
}

func (n *Node) Peers() []multiaddr.Multiaddr {
	res := n.Runner.MustRun(RunRequest{
		Path: n.IPFSBin,
		Args: []string{"swarm", "peers"},
	})
	lines := strings.Split(strings.TrimSpace(res.Stdout.String()), "\n")
	var addrs []multiaddr.Multiaddr
	for _, line := range lines {
		ma, err := multiaddr.NewMultiaddr(line)
		if err != nil {
			panic(err)
		}
		addrs = append(addrs, ma)
	}
	return addrs
}
