package sshc

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ferama/rospo/pkg/logger"
	"github.com/ferama/rospo/pkg/utils"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	"golang.org/x/term"
)

var log = logger.NewLogger("[SSHC] ", logger.Green)

// The ssh connection available statuses
const (
	STATUS_CONNECTING = "Connecting..."
	STATUS_CONNECTED  = "Connected"
	STATUS_CLOSED     = "Closed"
)

// SshConnection implements an ssh client
type SshConnection struct {
	username   string
	identity   string
	password   string
	knownHosts string

	serverEndpoint *utils.Endpoint

	insecure  bool
	quiet     bool
	jumpHosts []*JumpHostConf

	reconnectionInterval time.Duration
	keepAliveInterval    time.Duration

	Client *ssh.Client
	// used to inform the tunnels if this sshClient
	// is connected. Tunnels will wait on this waitGroup to
	// know if the ssh client is connected or not
	connected sync.WaitGroup

	connectionStatus   string
	connectionStatusMU sync.Mutex
	clientMU           sync.Mutex
	// indicates the connection status request
	isStopped atomic.Bool
}

// NewSshConnection creates a new SshConnection instance
func NewSshConnection(conf *SshClientConf) *SshConnection {

	parsed := utils.ParseSSHUrl(conf.ServerURI)
	var knownHostsPath string
	if conf.KnownHosts == "" {
		usr, _ := user.Current()
		knownHostsPath = filepath.Join(usr.HomeDir, ".ssh", "known_hosts")
	} else {
		knownHostsPath, _ = utils.ExpandUserHome(conf.KnownHosts)
	}

	c := &SshConnection{
		username:       parsed.Username,
		identity:       conf.Identity,
		password:       conf.Password,
		knownHosts:     knownHostsPath,
		serverEndpoint: conf.GetServerEndpoint(),
		insecure:       conf.Insecure,
		quiet:          conf.Quiet,
		jumpHosts:      conf.JumpHosts,

		keepAliveInterval:    5 * time.Second,
		reconnectionInterval: 5 * time.Second,
		connectionStatus:     STATUS_CONNECTING,
		isStopped:            atomic.Bool{},
	}

	c.isStopped.Store(true)
	// client is not connected on startup, so add 1 here
	c.connected.Add(1)
	if c.quiet {
		log.SetOutput(io.Discard)
	}

	return c
}

// Waits until the connection is estabilished with the server
func (s *SshConnection) ReadyWait() {
	s.connected.Wait()
}

// Stop closes the ssh conn instance client connection
func (s *SshConnection) Stop() {
	s.isStopped.Store(true)
	s.resetConn()
}

// resets the connection after a stop request or if it fails
func (s *SshConnection) resetConn() {
	s.clientMU.Lock()
	if s.Client != nil {
		s.Client.Close()
	}
	s.clientMU.Unlock()

	s.connectionStatusMU.Lock()
	s.connectionStatus = STATUS_CLOSED
	s.connectionStatusMU.Unlock()
}

// Start connects the ssh client to the remote server
// and keeps it connected sending keep alive packet
// and reconnecting in the event of network failures
func (s *SshConnection) Start() {
	s.isStopped.Store(false)
	for {
		// this becomes true if Stop() was called in the meantime
		if s.isStopped.Load() {
			break
		}
		s.connectionStatusMU.Lock()
		s.connectionStatus = STATUS_CONNECTING
		s.connectionStatusMU.Unlock()

		if err := s.connect(); err != nil {
			log.Printf("error while connecting %s", err)
			time.Sleep(s.reconnectionInterval)
			continue
		}
		// client connected. Free the wait group
		s.connected.Done()

		s.connectionStatusMU.Lock()
		s.connectionStatus = STATUS_CONNECTED
		s.connectionStatusMU.Unlock()

		// this call will block until the connection fails
		s.keepAlive()

		s.resetConn()
		s.connected.Add(1)
	}
}

// GetConnectionStatus returns the current connection status as a string
func (s *SshConnection) GetConnectionStatus() string {
	s.connectionStatusMU.Lock()
	defer s.connectionStatusMU.Unlock()
	return s.connectionStatus
}

// GrabPubKey is an helper function that gets server pubkey
func (s *SshConnection) GrabPubKey() {
	sshConfig := &ssh.ClientConfig{
		HostKeyCallback: s.verifyHostCallback(false),
	}
	// ignore return values here. I'm using it just to trigger the
	// verifyHostCallback
	ssh.Dial("tcp", s.serverEndpoint.String(), sshConfig)
}

func (s *SshConnection) keepAlive() {
	log.Println("starting client keep alive")
	for {
		// log.Println("keep alive")
		_, _, err := s.Client.SendRequest("keepalive@rospo", true, nil)
		if err != nil {
			log.Printf("error while sending keep alive %s", err)
			return
		}
		time.Sleep(s.keepAliveInterval)
	}
}
func (s *SshConnection) connect() error {
	sshConfig := &ssh.ClientConfig{
		// SSH connection username
		User:            s.username,
		Auth:            s.getAuthMethods(),
		HostKeyCallback: s.verifyHostCallback(true),
		BannerCallback: func(message string) error {
			if !s.quiet {
				fmt.Print(message)
			}
			return nil
		},
	}
	log.Println("trying to connect to remote server...")

	identityPath := s.identity
	if s.identity == "" {
		usr, _ := user.Current()
		identityPath = filepath.Join(usr.HomeDir, ".ssh", "id_rsa")
	}

	log.Printf("using identity at %s", identityPath)

	if len(s.jumpHosts) != 0 {
		client, err := s.jumpHostConnect(s.serverEndpoint, sshConfig)
		if err != nil {
			return err
		}
		s.clientMU.Lock()
		s.Client = client
		s.clientMU.Unlock()

	} else {
		client, err := s.directConnect(s.serverEndpoint, sshConfig)
		if err != nil {
			return err
		}
		s.clientMU.Lock()
		s.Client = client
		s.clientMU.Unlock()
	}

	return nil
}

func (s *SshConnection) verifyHostCallback(fail bool) ssh.HostKeyCallback {

	if s.insecure {
		return func(host string, remote net.Addr, key ssh.PublicKey) error {
			return nil
		}
	}
	return func(host string, remote net.Addr, key ssh.PublicKey) error {
		var err error

		log.Printf("using known_hosts file at %s", s.knownHosts)

		clb, err := knownhosts.New(s.knownHosts)
		if err != nil {
			log.Printf("error while parsing 'known_hosts' file: %s: %v", s.knownHosts, err)
			f, fErr := os.OpenFile(s.knownHosts, os.O_CREATE, 0600)
			if fErr != nil {
				log.Fatalf("%s", fErr)
			}
			f.Close()
			clb, err = knownhosts.New(s.knownHosts)
			if err != nil {
				log.Fatalf("%s", err)
			}
		}
		var keyErr *knownhosts.KeyError
		e := clb(host, remote, key)
		if errors.As(e, &keyErr) && len(keyErr.Want) > 0 {
			log.Printf("ERROR: %s is not a key of %s, either a man in the middle attack or %s host pub key was changed.", ssh.FingerprintSHA256(key), host, host)
			return e
		} else if errors.As(e, &keyErr) && len(keyErr.Want) == 0 {
			if fail {
				log.Fatalf(`ERROR: the host '%s' is not trusted. If it is trusted instead, 
				  please grab its pub key using the 'rospo grabpubkey' command`, host)
				return errors.New("")
			}
			log.Printf("WARNING: %s is not trusted, adding this key: \n\n%s\n\nto known_hosts file.", host, utils.SerializePublicKey(key))
			return utils.AddHostKeyToKnownHosts(host, key, s.knownHosts)
		}
		return e
	}
}

func (s *SshConnection) getAuthMethods() []ssh.AuthMethod {
	authMethods := []ssh.AuthMethod{}

	keysAuth, err := utils.LoadIdentityFile(s.identity)
	if err == nil {
		authMethods = append(authMethods, keysAuth)
	}
	if s.password != "" {
		authMethods = append(authMethods, ssh.Password(s.password))
	}

	authMethods = append(authMethods, ssh.PasswordCallback(func() (secret string, err error) {
		fmt.Println("\nThe server asks for a password")
		fmt.Println("Password: ")
		p, err := term.ReadPassword(0)
		return string(p), err
	}))

	return authMethods
}

func (s *SshConnection) jumpHostConnect(
	server *utils.Endpoint,
	sshConfig *ssh.ClientConfig,
) (*ssh.Client, error) {

	var (
		jhClient *ssh.Client
		jhConn   net.Conn
		err      error
	)

	// traverse all the hops
	for idx, jh := range s.jumpHosts {
		parsed := utils.ParseSSHUrl(jh.URI)
		hop := &utils.Endpoint{
			Host: parsed.Host,
			Port: parsed.Port,
		}

		config := &ssh.ClientConfig{
			User:            parsed.Username,
			Auth:            s.getAuthMethods(),
			HostKeyCallback: s.verifyHostCallback(true),
		}
		log.Printf("connecting to hop %s@%s", parsed.Username, hop.String())

		// if it is the first hop, use ssh Dial to create the first client
		if idx == 0 {
			jhClient, err = ssh.Dial("tcp", hop.String(), config)
			if err != nil {
				log.Printf("dial INTO remote server error. %s", err)
				return nil, err
			}
		} else {
			jhConn, err = jhClient.Dial("tcp", hop.String())
			if err != nil {
				return nil, err
			}
			ncc, chans, reqs, err := ssh.NewClientConn(jhConn, hop.String(), config)
			if err != nil {
				return nil, err
			}
			jhClient = ssh.NewClient(ncc, chans, reqs)
		}
		log.Printf("reached the jump host %s@%s", parsed.Username, hop.String())
	}

	// now I'm ready to reach the final hop, the server
	log.Printf("connecting to %s@%s", sshConfig.User, server.String())
	jhConn, err = jhClient.Dial("tcp", server.String())
	if err != nil {
		return nil, err
	}
	ncc, chans, reqs, err := ssh.NewClientConn(jhConn, server.String(), sshConfig)
	if err != nil {
		return nil, err
	}
	client := ssh.NewClient(ncc, chans, reqs)

	return client, nil
}

func (s *SshConnection) directConnect(
	server *utils.Endpoint,
	sshConfig *ssh.ClientConfig,
) (*ssh.Client, error) {

	log.Printf("connecting to %s", server.String())
	client, err := ssh.Dial("tcp", server.String(), sshConfig)
	if err != nil {
		log.Printf("dial INTO remote server error. %s", err)
		return nil, err
	}
	log.Printf("connected to remote server at %s\n", server.String())
	return client, nil
}
