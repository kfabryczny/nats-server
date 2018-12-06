// Copyright 2018 The NATS Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultSolicitGatewaysDelay         = time.Second
	defaultGatewayConnectDelay          = time.Second
	defaultGatewayReconnectDelay        = time.Second
	defaultGatewayMaxRUnsubBeforeSwitch = 1000
)

var (
	gatewayConnectDelay          = defaultGatewayConnectDelay
	gatewayReconnectDelay        = defaultGatewayReconnectDelay
	gatewayMaxRUnsubBeforeSwitch = defaultGatewayMaxRUnsubBeforeSwitch
)

const (
	gatewayCmdGossip          byte = 1
	gatewayCmdAllSubsStart    byte = 2
	gatewayCmdAllSubsComplete byte = 3
)

// Gateway modes
const (
	// modeOptimistic is the default mode where a cluster will send
	// to a gateway unless it is been told that there is no interest
	// (this is for plain subscribers only).
	modeOptimistic = byte(iota)
	// modeTransitioning is when a gateway has to send too many
	// no interest on subjects to the remote and decides that it is
	// now time to move to modeInterestOnly (this is on a per account
	// basis).
	modeTransitioning
	// modeInterestOnly means that a cluster sends all it subscriptions
	// interest to the gateway, which in return does not send a message
	// unless it knows that there is explicit interest.
	modeInterestOnly
)

type srvGateway struct {
	totalQSubs int64 //total number of queue subs in all remote gateways (used with atomic operations)
	sync.RWMutex
	enabled  bool                   // Immutable, true if both a name and port are configured
	name     string                 // Name of the Gateway on this server
	out      map[string]*client     // outbound gateways
	outo     []*client              // outbound gateways maintained in an order suitable for sending msgs (currently based on RTT)
	in       map[uint64]*client     // inbound gateways
	remotes  map[string]*gatewayCfg // Config of remote gateways
	URLs     map[string]struct{}    // Set of all Gateway URLs in the cluster
	URL      string                 // This server gateway URL (after possible random port is resolved)
	info     *Info                  // Gateway Info protocol
	infoJSON []byte                 // Marshal'ed Info protocol
	defPerms *GatewayPermissions    // Default permissions (when accepting an unknown remote gateway)
	runknown bool                   // Rejects unknown (not configured) gateway connections

	// We maintain the interest of subjects and queues per account.
	// For a given account, entries in the map could be something like this:
	// foo.bar {n: 3} 			// 3 subs on foo.bar
	// foo.>   {n: 6}			// 6 subs on foo.>
	// foo bar {n: 1, q: true}  // 1 qsub on foo, queue bar
	// foo baz {n: 3, q: true}  // 3 qsubs on foo, queue baz
	pasi struct {
		// Protect map since accessed from different go-routine and avoid
		// possible race resulting in RS+ being sent before RS- resulting
		// in incorrect interest suppression.
		// Will use while sending QSubs (on GW connection accept) and when
		// switching to the send-all-subs mode.
		sync.Mutex
		m map[string]map[string]*sitally
	}

	resolver netResolver // Used to resolve host name before calling net.Dial()
	sqbsz    int         // Max buffer size to send queue subs protocol. Used for testing.
}

// Subject interest tally. Also indicates if the key in the map is a
// queue or not.
type sitally struct {
	n int32 // number of subscriptions directly matching
	q bool  // indicate that this is a queue
}

type gatewayCfg struct {
	sync.RWMutex
	*RemoteGatewayOpts
	urls         map[string]*url.URL
	connAttempts int
	implicit     bool
	tlsName      string
}

// Struct for client's gateway related fields
type gateway struct {
	name       string
	outbound   bool
	cfg        *gatewayCfg
	connectURL *url.URL                      // Needed when sending CONNECT after receiving INFO from remote
	infoJSON   []byte                        // Needed when sending INFO after receiving INFO from remote
	outsim     *sync.Map                     // Per-account subject interest (or no-interest) (outbound conn)
	insim      map[string]*insie             // Per-account subject no-interest sent or send-all-subs mode (inbound conn)
	replySubs  map[*subscription]*time.Timer // Same than replySubs in client.route
}

// Outbound subject interest entry.
type outsie struct {
	sync.RWMutex
	// Indicate that all subs should be stored. This is
	// set to true when receiving the command from the
	// remote that we are about to receive all its subs.
	mode byte
	// If not nil, used for no-interest for plain subs.
	// If a subject is present in this map, it means that
	// the remote is not interested in that subject.
	// When we have received the command that says that
	// the remote has sent all its subs, this is set to nil.
	ni map[string]struct{}
	// Contains queue subscriptions when in optimistic mode,
	// and all subs when pk is > 0.
	sl *Sublist
}

// Inbound subject interest entry.
// If `ni` is not nil, it stores the subjects for which an
// RS- was sent to the remote gateway. When a subscription
// is created, this is used to know if we need to send
// an RS+ to clear the no-interest in the remote.
// When an account is switched to modeInterestOnly (we send
// all subs of an account to the remote), then `ni` is nil and
// when all subs have been sent, `sap` is set to true.
type insie struct {
	ni   map[string]struct{} // Record if RS- was sent for given subject
	mode byte                // modeOptimistic or modeInterestOnly
}

// clone returns a deep copy of the RemoteGatewayOpts object
func (r *RemoteGatewayOpts) clone() *RemoteGatewayOpts {
	if r == nil {
		return nil
	}
	clone := &RemoteGatewayOpts{
		Name: r.Name,
		URLs: deepCopyURLs(r.URLs),
	}
	if r.TLSConfig != nil {
		clone.TLSConfig = r.TLSConfig.Clone()
		clone.TLSTimeout = r.TLSTimeout
	}
	return clone
}

// Ensure that gateway is properly configured.
func validateGatewayOptions(o *Options) error {
	if o.Gateway.Name == "" && o.Gateway.Port == 0 {
		return nil
	}
	if o.Gateway.Name == "" {
		return fmt.Errorf("gateway has no name")
	}
	if o.Gateway.Port == 0 {
		return fmt.Errorf("gateway %q has no port specified (select -1 for random port)", o.Gateway.Name)
	}
	for i, g := range o.Gateway.Gateways {
		if g.Name == "" {
			return fmt.Errorf("gateway in the list %d has no name", i)
		}
		if len(g.URLs) == 0 {
			return fmt.Errorf("gateway %q has no URL", g.Name)
		}
	}
	return nil
}

// Initialize the s.gateway structure. We do this even if the server
// does not have a gateway configured. In some part of the code, the
// server will check the number of outbound gateways, etc.. and so
// we don't have to check if s.gateway is nil or not.
func newGateway(opts *Options) (*srvGateway, error) {
	gateway := &srvGateway{
		name:     opts.Gateway.Name,
		out:      make(map[string]*client),
		outo:     make([]*client, 0, 4),
		in:       make(map[uint64]*client),
		remotes:  make(map[string]*gatewayCfg),
		URLs:     make(map[string]struct{}),
		resolver: opts.Gateway.resolver,
		runknown: opts.Gateway.RejectUnknown,
	}
	gateway.Lock()
	defer gateway.Unlock()

	gateway.pasi.m = make(map[string]map[string]*sitally)

	if gateway.resolver == nil {
		gateway.resolver = netResolver(net.DefaultResolver)
	}

	// Copy default permissions (works if DefaultPermissions is nil)
	gateway.defPerms = opts.Gateway.DefaultPermissions.clone()

	// Create remote gateways
	for _, rgo := range opts.Gateway.Gateways {
		// Ignore if there is a remote gateway with our name.
		if rgo.Name == gateway.name {
			continue
		}
		cfg := &gatewayCfg{
			RemoteGatewayOpts: rgo.clone(),
			urls:              make(map[string]*url.URL, len(rgo.URLs)),
		}
		if opts.Gateway.TLSConfig != nil && cfg.TLSConfig == nil {
			cfg.TLSConfig = opts.Gateway.TLSConfig.Clone()
		}
		if cfg.TLSTimeout == 0 {
			cfg.TLSTimeout = opts.Gateway.TLSTimeout
		}
		for _, u := range rgo.URLs {
			// For TLS, look for a hostname that we can use for TLSConfig.ServerName
			cfg.saveTLSHostname(u)
			cfg.urls[u.Host] = u
		}
		gateway.remotes[cfg.Name] = cfg
	}

	gateway.sqbsz = opts.Gateway.sendQSubsBufSize
	if gateway.sqbsz == 0 {
		gateway.sqbsz = maxBufSize
	}

	gateway.enabled = opts.Gateway.Name != "" && opts.Gateway.Port != 0
	return gateway, nil
}

// Returns the Gateway's name of this server.
func (g *srvGateway) getName() string {
	g.RLock()
	n := g.name
	g.RUnlock()
	return n
}

// Returns the Gateway URLs of all servers in the local cluster.
// This is used to send to other cluster this server connects to.
// The gateway read-lock is held on entry
func (g *srvGateway) getURLs() []string {
	a := make([]string, 0, len(g.URLs))
	for u := range g.URLs {
		a = append(a, u)
	}
	return a
}

// Returns if this server rejects connections from gateways that are not
// explicitly configured.
func (g *srvGateway) rejectUnknown() bool {
	g.RLock()
	reject := g.runknown
	g.RUnlock()
	return reject
}

// Starts the gateways accept loop and solicit explicit gateways
// after an initial delay. This delay is meant to give a chance to
// the cluster to form and this server gathers gateway URLs for this
// cluster in order to send that as part of the connect/info process.
func (s *Server) startGateways() {
	// Spin up the accept loop
	ch := make(chan struct{})
	go s.gatewayAcceptLoop(ch)
	<-ch

	// Delay start of creation of gateways to give a chance
	// to the local cluster to form.
	s.startGoRoutine(func() {
		defer s.grWG.Done()

		dur := s.getOpts().gatewaysSolicitDelay
		if dur == 0 {
			dur = defaultSolicitGatewaysDelay
		}

		select {
		case <-time.After(dur):
			s.solicitGateways()
		case <-s.quitCh:
			return
		}
	})
}

// This is the gateways accept loop. This runs as a go-routine.
// The listen specification is resolved (if use of random port),
// then a listener is started. After that, this routine enters
// a loop (until the server is shutdown) accepting incoming
// gateway connections.
func (s *Server) gatewayAcceptLoop(ch chan struct{}) {
	defer func() {
		if ch != nil {
			close(ch)
		}
	}()

	// Snapshot server options.
	opts := s.getOpts()

	port := opts.Gateway.Port
	if port == -1 {
		port = 0
	}

	hp := net.JoinHostPort(opts.Gateway.Host, strconv.Itoa(port))
	l, e := net.Listen("tcp", hp)
	if e != nil {
		s.Fatalf("Error listening on gateway port: %d - %v", opts.Gateway.Port, e)
		return
	}
	s.Noticef("Gateway name is %s", s.getGatewayName())
	s.Noticef("Listening for gateways connections on %s",
		net.JoinHostPort(opts.Gateway.Host, strconv.Itoa(l.Addr().(*net.TCPAddr).Port)))

	s.mu.Lock()
	tlsReq := opts.Gateway.TLSConfig != nil
	authRequired := opts.Gateway.Username != ""
	info := &Info{
		ID:           s.info.ID,
		Version:      s.info.Version,
		AuthRequired: authRequired,
		TLSRequired:  tlsReq,
		TLSVerify:    tlsReq,
		MaxPayload:   s.info.MaxPayload,
		Gateway:      opts.Gateway.Name,
	}
	// If we have selected a random port...
	if port == 0 {
		// Write resolved port back to options.
		opts.Gateway.Port = l.Addr().(*net.TCPAddr).Port
	}
	// Keep track of actual listen port. This will be needed in case of
	// config reload.
	s.gatewayActualPort = opts.Gateway.Port
	// Possibly override Host/Port based on Gateway.Advertise
	if err := s.setGatewayInfoHostPort(info, opts); err != nil {
		s.Fatalf("Error setting gateway INFO with Gateway.Advertise value of %s, err=%v", opts.Gateway.Advertise, err)
		l.Close()
		s.mu.Unlock()
		return
	}
	// Setup state that can enable shutdown
	s.gatewayListener = l
	s.mu.Unlock()

	// Let them know we are up
	close(ch)
	ch = nil

	tmpDelay := ACCEPT_MIN_SLEEP

	for s.isRunning() {
		conn, err := l.Accept()
		if err != nil {
			tmpDelay = s.acceptError("Gateway", err, tmpDelay)
			continue
		}
		tmpDelay = ACCEPT_MIN_SLEEP
		s.startGoRoutine(func() {
			s.createGateway(nil, nil, conn)
			s.grWG.Done()
		})
	}
	s.Debugf("Gateway accept loop exiting..")
	s.done <- true
}

// Similar to setInfoHostPortAndGenerateJSON, but for gatewayInfo.
func (s *Server) setGatewayInfoHostPort(info *Info, o *Options) error {
	if o.Gateway.Advertise != "" {
		advHost, advPort, err := parseHostPort(o.Gateway.Advertise, o.Gateway.Port)
		if err != nil {
			return err
		}
		info.Host = advHost
		info.Port = advPort
	} else {
		info.Host = o.Gateway.Host
		info.Port = o.Gateway.Port
	}
	gw := s.gateway
	gw.Lock()
	delete(gw.URLs, gw.URL)
	gw.URL = net.JoinHostPort(info.Host, strconv.Itoa(info.Port))
	gw.URLs[gw.URL] = struct{}{}
	gw.info = info
	info.GatewayURL = gw.URL
	// (re)generate the gatewayInfoJSON byte array
	gw.generateInfoJSON()
	gw.Unlock()
	return nil
}

// Generates the Gateway INFO protocol.
// The gateway lock is held on entry
func (g *srvGateway) generateInfoJSON() {
	b, err := json.Marshal(g.info)
	if err != nil {
		panic(err)
	}
	g.infoJSON = []byte(fmt.Sprintf(InfoProto, b))
}

// Goes through the list of registered gateways and try to connect to those.
// The list (remotes) is initially containing the explicit remote gateways,
// but the list is augmented with any implicit (discovered) gateway. Therefore,
// this function only solicit explicit ones.
func (s *Server) solicitGateways() {
	gw := s.gateway
	gw.RLock()
	defer gw.RUnlock()
	for _, cfg := range gw.remotes {
		// Since we delay the creation of gateways, it is
		// possible that server starts to receive inbound from
		// other clusters and in turn create outbounds. So here
		// we create only the ones that are configured.
		if !cfg.isImplicit() {
			cfg := cfg // Create new instance for the goroutine.
			s.startGoRoutine(func() {
				s.solicitGateway(cfg)
				s.grWG.Done()
			})
		}
	}
}

// Reconnect to the gateway after a little wait period. For explicit
// gateways, we also wait for the default reconnect time.
func (s *Server) reconnectGateway(cfg *gatewayCfg) {
	defer s.grWG.Done()

	delay := time.Duration(rand.Intn(100)) * time.Millisecond
	if !cfg.isImplicit() {
		delay += gatewayReconnectDelay
	}
	select {
	case <-time.After(delay):
	case <-s.quitCh:
		return
	}
	s.solicitGateway(cfg)
}

// This function will loop trying to connect to any URL attached
// to the given Gateway. It will return once a connection has been created.
func (s *Server) solicitGateway(cfg *gatewayCfg) {
	var (
		opts       = s.getOpts()
		isImplicit = cfg.isImplicit()
		urls       = cfg.getURLs()
		attempts   int
	)
	for s.isRunning() && len(urls) > 0 {
		// Iteration is random
		for _, u := range urls {
			address, err := s.getRandomIP(s.gateway.resolver, u.Host)
			if err != nil {
				s.Errorf("Error getting IP for %s: %v", u.Host, err)
				continue
			}
			s.Debugf("Trying to connect to gateway %q at %s", cfg.Name, address)
			conn, err := net.DialTimeout("tcp", address, DEFAULT_ROUTE_DIAL)
			if err == nil {
				// We could connect, create the gateway connection and return.
				s.createGateway(cfg, u, conn)
				return
			}
			s.Errorf("Error trying to connect to gateway: %v", err)
			// Break this loop if server is being shutdown...
			if !s.isRunning() {
				break
			}
		}
		if isImplicit {
			attempts++
			if opts.Gateway.ConnectRetries == 0 || attempts > opts.Gateway.ConnectRetries {
				s.gateway.Lock()
				delete(s.gateway.remotes, cfg.Name)
				s.gateway.Unlock()
				return
			}
		}
		select {
		case <-s.quitCh:
			return
		case <-time.After(gatewayConnectDelay):
			continue
		}
	}
}

// Called when a gateway connection is either accepted or solicited.
// If accepted, the gateway is marked as inbound.
// If solicited, the gateway is marked as outbound.
func (s *Server) createGateway(cfg *gatewayCfg, url *url.URL, conn net.Conn) {
	// Snapshot server options.
	opts := s.getOpts()

	c := &client{srv: s, nc: conn, kind: GATEWAY}

	// Are we creating the gateway based on the configuration
	solicit := cfg != nil
	var tlsRequired bool
	if solicit {
		tlsRequired = cfg.TLSConfig != nil
	} else {
		tlsRequired = opts.Gateway.TLSConfig != nil
	}

	// Generate INFO to send
	s.gateway.RLock()
	// Make a copy
	info := *s.gateway.info
	info.GatewayURLs = s.gateway.getURLs()
	s.gateway.RUnlock()
	b, _ := json.Marshal(&info)
	infoJSON := []byte(fmt.Sprintf(InfoProto, b))

	// Perform some initialization under the client lock
	c.mu.Lock()
	c.initClient()
	c.gw = &gateway{}
	c.in.pacache = make(map[string]*perAccountCache, maxPerAccountCacheSize)
	if solicit {
		// This is an outbound gateway connection
		c.gw.outbound = true
		c.gw.name = cfg.Name
		c.gw.cfg = cfg
		cfg.bumpConnAttempts()
		// Since we are delaying the connect until after receiving
		// the remote's INFO protocol, save the URL we need to connect to.
		c.gw.connectURL = url
		c.gw.infoJSON = infoJSON
		c.gw.outsim = &sync.Map{}
		c.Noticef("Creating outbound gateway connection to %q", cfg.Name)
	} else {
		// Inbound gateway connection
		c.gw.insim = make(map[string]*insie)
		c.Noticef("Processing inbound gateway connection")
	}

	// Check for TLS
	if tlsRequired {
		var timeout float64
		// If we solicited, we will act like the client, otherwise the server.
		if solicit {
			c.Debugf("Starting TLS gateway client handshake")
			cfg.RLock()
			tlsName := cfg.tlsName
			tlsConfig := cfg.TLSConfig
			timeout = cfg.TLSTimeout
			cfg.RUnlock()
			// If the given url is a hostname, use this hostname for the
			// ServerName. If it is an IP, use the cfg's tlsName. If none
			// is available, resort to current IP.
			host := url.Hostname()
			if tlsName != "" && net.ParseIP(host) != nil {
				host = tlsName
			}
			tlsConfig.ServerName = host
			c.nc = tls.Client(c.nc, tlsConfig)
		} else {
			c.Debugf("Starting TLS gateway server handshake")
			c.nc = tls.Server(c.nc, opts.Gateway.TLSConfig)
			timeout = opts.Gateway.TLSTimeout
		}

		conn := c.nc.(*tls.Conn)

		// Setup the timeout
		ttl := secondsToDuration(timeout)
		time.AfterFunc(ttl, func() { tlsTimeout(c, conn) })
		conn.SetReadDeadline(time.Now().Add(ttl))

		c.mu.Unlock()
		if err := conn.Handshake(); err != nil {
			c.Errorf("TLS gateway handshake error: %v", err)
			c.sendErr("Secure Connection - TLS Required")
			c.closeConnection(TLSHandshakeError)
			return
		}
		// Reset the read deadline
		conn.SetReadDeadline(time.Time{})

		// Re-Grab lock
		c.mu.Lock()

		// Verify that the connection did not go away while we released the lock.
		if c.nc == nil {
			c.mu.Unlock()
			return
		}
	}

	// Do final client initialization

	// Register in temp map for now until gateway properly registered
	// in out or in gateways.
	if !s.addToTempClients(c.cid, c) {
		c.mu.Unlock()
		c.closeConnection(ServerShutdown)
		return
	}

	// Only send if we accept a connection. Will send CONNECT+INFO as an
	// outbound only after processing peer's INFO protocol.
	if !solicit {
		c.sendInfo(infoJSON)
	}

	// Spin up the read loop.
	s.startGoRoutine(c.readLoop)

	// Spin up the write loop.
	s.startGoRoutine(c.writeLoop)

	if tlsRequired {
		c.Debugf("TLS handshake complete")
		cs := c.nc.(*tls.Conn).ConnectionState()
		c.Debugf("TLS version %s, cipher suite %s", tlsVersion(cs.Version), tlsCipher(cs.CipherSuite))
	}

	// Set the Ping timer after sending connect and info.
	c.setPingTimer()

	c.mu.Unlock()
}

// Builds and sends the CONNET protocol for a gateway.
func (c *client) sendGatewayConnect() {
	tlsRequired := c.gw.cfg.TLSConfig != nil
	url := c.gw.connectURL
	c.gw.connectURL = nil
	var user, pass string
	if userInfo := url.User; userInfo != nil {
		user = userInfo.Username()
		pass, _ = userInfo.Password()
	}
	cinfo := connectInfo{
		Verbose:  false,
		Pedantic: false,
		User:     user,
		Pass:     pass,
		TLS:      tlsRequired,
		Name:     c.srv.info.ID,
		Gateway:  c.srv.getGatewayName(),
	}
	b, err := json.Marshal(cinfo)
	if err != nil {
		panic(err)
	}
	c.sendProto([]byte(fmt.Sprintf(ConProto, b)), true)
}

// Process the CONNECT protocol from a gateway connection.
// Returns an error to the connection if the CONNECT is not from a gateway
// (for instance a client or route connecting to the gateway port), or
// if the destination does not match the gateway name of this server.
//
// <Invoked from inbound connection's readLoop>
func (c *client) processGatewayConnect(arg []byte) error {
	connect := &connectInfo{}
	if err := json.Unmarshal(arg, connect); err != nil {
		return err
	}

	// Coming from a client or a route, reject
	if connect.Gateway == "" {
		c.sendErrAndErr(ErrClientOrRouteConnectedToGatewayPort.Error())
		c.closeConnection(WrongPort)
		return ErrClientOrRouteConnectedToGatewayPort
	}

	c.mu.Lock()
	s := c.srv
	c.mu.Unlock()

	// If we reject unknown gateways, make sure we have it configured,
	// otherwise return an error.
	if s.gateway.rejectUnknown() && s.getRemoteGateway(connect.Gateway) == nil {
		c.Errorf("Rejecting connection from gateway %q", connect.Gateway)
		c.sendErr(fmt.Sprintf("Connection to gateway %q rejected", s.getGatewayName()))
		c.closeConnection(WrongGateway)
		return ErrWrongGateway
	}

	return nil
}

// Process the INFO protocol from a gateway connection.
//
// If the gateway connection is an outbound (this server initiated the connection),
// this function checks that the incoming INFO contains the Gateway field. If empty,
// it means that this is a response from an older server or that this server connected
// to the wrong port.
// The outbound gateway may also receive a gossip INFO protocol from the remote gateway,
// indicating other gateways that the remote knows about. This server will try to connect
// to those gateways (if not explicitly configured or already implicitly connected).
// In both cases (explicit or implicit), the local cluster is notified about the existence
// of this new gateway. This allows servers in the cluster to ensure that they have an
// outbound connection to this gateway.
//
// For an inbound gateway, the gateway is simply registered and the info protocol
// is saved to be used after processing the CONNECT.
//
// <Invoked from both inbound/outbound readLoop's connection>
func (c *client) processGatewayInfo(info *Info) {
	var (
		gwName string
		cfg    *gatewayCfg
	)
	c.mu.Lock()
	s := c.srv
	cid := c.cid

	// Check if this is the first INFO. (this call sets the flag if not already set).
	isFirstINFO := c.flags.setIfNotSet(infoReceived)

	isOutbound := c.gw.outbound
	if isOutbound {
		gwName = c.gw.name
		cfg = c.gw.cfg
	} else if isFirstINFO {
		c.gw.name = info.Gateway
	}
	if isFirstINFO {
		c.opts.Name = info.ID
	}
	c.mu.Unlock()

	// For an outbound connection...
	if isOutbound {
		// Check content of INFO for fields indicating that it comes from a gateway.
		// If we incorrectly connect to the wrong port (client or route), we won't
		// have the Gateway field set.
		if info.Gateway == "" {
			c.sendErrAndErr(fmt.Sprintf("Attempt to connect to gateway %q using wrong port", gwName))
			c.closeConnection(WrongPort)
			return
		}
		// Check that the gateway name we got is what we expect
		if info.Gateway != gwName {
			// Unless this is the very first INFO, it may be ok if this is
			// a gossip request to connect to other gateways.
			if !isFirstINFO && info.GatewayCmd == gatewayCmdGossip {
				// If we are configured to reject unknown, do not attempt to
				// connect to one that we don't have configured.
				if s.gateway.rejectUnknown() && s.getRemoteGateway(info.Gateway) == nil {
					return
				}
				s.processImplicitGateway(info)
				return
			}
			// Otherwise, this is a failure...
			// We are reporting this error in the log...
			c.Errorf("Failing connection to gateway %q, remote gateway name is %q",
				gwName, info.Gateway)
			// ...and sending this back to the remote so that the error
			// makes more sense in the remote server's log.
			c.sendErr(fmt.Sprintf("Connection from %q rejected, wanted to connect to %q, this is %q",
				s.getGatewayName(), gwName, info.Gateway))
			c.closeConnection(WrongGateway)
			return
		}

		// Possibly add URLs that we get from the INFO protocol.
		if len(info.GatewayURLs) > 0 {
			cfg.updateURLs(info.GatewayURLs)
		}

		// If this is the first INFO, send our connect
		if isFirstINFO {
			// Note, if we want to support NKeys, then we would get the nonce
			// from this INFO protocol and can sign it in the CONNECT we are
			// going to send now.
			c.mu.Lock()
			c.sendGatewayConnect()
			c.Debugf("Gateway connect protocol sent to %q", gwName)
			// Send INFO too
			c.sendInfo(c.gw.infoJSON)
			c.gw.infoJSON = nil
			c.mu.Unlock()

			// Register as an outbound gateway.. if we had a protocol to ack our connect,
			// then we should do that when process that ack.
			s.registerOutboundGatewayConnection(gwName, c)
			c.Noticef("Outbound gateway connection to %q (%s) registered", gwName, info.ID)
			// Now that the outbound gateway is registered, we can remove from temp map.
			s.removeFromTempClients(cid)
		} else if info.GatewayCmd > 0 {
			switch info.GatewayCmd {
			case gatewayCmdAllSubsStart:
				c.gatewayAllSubsReceiveStart(info)
				return
			case gatewayCmdAllSubsComplete:
				c.gatewayAllSubsReceiveComplete(info)
				return
			default:
				s.Warnf("Received unknown command %v from gateway %q", info.GatewayCmd, gwName)
				return
			}
		}

		// Flood local cluster with information about this gateway.
		// Servers in this cluster will ensure that they have (or otherwise create)
		// an outbound connection to this gateway.
		s.forwardNewGatewayToLocalCluster(info)

	} else if isFirstINFO {
		// This is the first INFO of an inbound connection...

		s.registerInboundGatewayConnection(cid, c)
		c.Noticef("Inbound gateway connection from %q (%s) registered", info.Gateway, info.ID)

		// Now that it is registered, we can remove from temp map.
		s.removeFromTempClients(cid)

		// Send our QSubs, since this may take some time, execute
		// in a separate go-routine so that if there is incoming
		// data from the otherside, we don't cause a slow consumer
		// error.
		s.startGoRoutine(func() {
			s.sendQueueSubsToGateway(c)
			s.grWG.Done()
		})

		// Initiate outbound connection. This function will behave correctly if
		// we have already one.
		s.processImplicitGateway(info)

		// Send back to the server that initiated this gateway connection the
		// list of all remote gateways known on this server.
		s.gossipGatewaysToInboundGateway(info.Gateway, c)
	}
}

// Sends to the given inbound gateway connection a gossip INFO protocol
// for each gateway known by this server. This allows for a "full mesh"
// of gateways.
func (s *Server) gossipGatewaysToInboundGateway(gwName string, c *client) {
	gw := s.gateway
	gw.RLock()
	defer gw.RUnlock()
	for gwCfgName, cfg := range gw.remotes {
		// Skip the gateway that we just created
		if gwCfgName == gwName {
			continue
		}
		info := Info{
			ID:         s.info.ID,
			GatewayCmd: gatewayCmdGossip,
		}
		urls := cfg.getURLsAsStrings()
		if len(urls) > 0 {
			info.Gateway = gwCfgName
			info.GatewayURLs = urls
			b, _ := json.Marshal(&info)
			c.mu.Lock()
			c.sendProto([]byte(fmt.Sprintf(InfoProto, b)), true)
			c.mu.Unlock()
		}
	}
}

// Sends the INFO protocol of a gateway to all routes known by this server.
func (s *Server) forwardNewGatewayToLocalCluster(oinfo *Info) {
	// Need to protect s.routes here, so use server's lock
	s.mu.Lock()
	defer s.mu.Unlock()

	// We don't really need the ID to be set, but, we need to make sure
	// that it is not set to the server ID so that if we were to connect
	// to an older server that does not expect a "gateway" INFO, it
	// would think that it needs to create an implicit route (since info.ID
	// would not match the route's remoteID), but will fail to do so because
	// the sent protocol will not have host/port defined.
	info := &Info{
		ID:          "GW" + s.info.ID,
		Gateway:     oinfo.Gateway,
		GatewayURLs: oinfo.GatewayURLs,
		GatewayCmd:  gatewayCmdGossip,
	}
	b, _ := json.Marshal(info)
	infoJSON := []byte(fmt.Sprintf(InfoProto, b))

	for _, r := range s.routes {
		r.mu.Lock()
		r.sendInfo(infoJSON)
		r.mu.Unlock()
	}
}

// Sends queue subscriptions interest to remote gateway.
// This is sent from the inbound side, that is, the side that receives
// messages from the remote's outbound connection. This side is
// the one sending the subscription interest.
func (s *Server) sendQueueSubsToGateway(c *client) {
	s.sendSubsToGateway(c, nil)
}

// Sends all subscriptions for the given account to the remove gateway
// This is sent from the inbound side, that is, the side that receives
// messages from the remote's outbound connection. This side is
// the one sending the subscription interest.
func (s *Server) sendAccountSubsToGateway(c *client, accName []byte) {
	s.sendSubsToGateway(c, accName)
}

// Sends subscriptions to remote gateway.
func (s *Server) sendSubsToGateway(c *client, accountName []byte) {
	var (
		bufa = [32 * 1024]byte{}
		buf  = bufa[:0]
		epa  = [1024]int{}
		ep   = epa[:0]
	)

	gw := s.gateway

	// This needs to run under this lock for the whole duration
	gw.pasi.Lock()
	defer gw.pasi.Unlock()

	// Build the protocols
	buildProto := func(accName []byte, acc map[string]*sitally, doQueues bool) {
		for saq, si := range acc {
			if doQueues && si.q || !doQueues && !si.q {
				buf = append(buf, rSubBytes...)
				buf = append(buf, accName...)
				buf = append(buf, ' ')
				// For queue subs (si.q is true), saq will be
				// subject + ' ' + queue, for plain subs, this is
				// just the subject.
				buf = append(buf, saq...)
				if doQueues {
					buf = append(buf, ' ', '1')
				}
				buf = append(buf, CR_LF...)
				ep = append(ep, len(buf))
			}
		}
	}
	// If account is specified...
	if accountName != nil {
		// Simply send all plain subs (no queues) for this specific account
		buildProto(accountName, gw.pasi.m[string(accountName)], false)
		// Instruct to send all subs (RS+/-) for this account from now on.
		c.mu.Lock()
		e := c.gw.insim[string(accountName)]
		if e != nil {
			e.mode = modeInterestOnly
		}
		c.mu.Unlock()
	} else {
		// Send queues for all accounts
		for accName, acc := range gw.pasi.m {
			buildProto([]byte(accName), acc, true)
		}
	}

	// Nothing to send.
	if len(buf) == 0 {
		return
	}
	if len(buf) > cap(bufa) {
		s.Debugf("Sending subscriptions to %q, buffer size: %v", c.gw.name, len(buf))
	}
	// Send
	mbs := gw.sqbsz
	mp := int(c.out.mp / 2)
	if mbs > mp {
		mbs = mp
	}
	le := 0
	li := 0
	for i := 0; i < len(ep); i++ {
		if ep[i]-le > mbs {
			var end int
			// If single proto is bigger than our max buffer size,
			// send anyway. If it reaches a max in queueOutbound,
			// that function will close the connection.
			if i-li <= 1 {
				end = ep[i]
			} else {
				end = ep[i-1]
				i--
			}
			c.mu.Lock()
			c.queueOutbound(buf[le:end])
			c.flushOutbound()
			closed := c.flags.isSet(clearConnection)
			c.mu.Unlock()
			if closed {
				return
			}
			le = ep[i]
			li = i
		}
	}
	c.mu.Lock()
	c.queueOutbound(buf[le:])
	c.flushOutbound()
	if !c.flags.isSet(clearConnection) {
		c.Debugf("Sent queue subscriptions to gateway")
	}
	c.mu.Unlock()
}

// This is invoked when getting an INFO protocol for gateway on the ROUTER port.
// This function will then execute appropriate function based on the command
// contained in the protocol.
// <Invoked from a route connection's readLoop>
func (s *Server) processGatewayInfoFromRoute(info *Info, routeSrvID string, route *client) {
	switch info.GatewayCmd {
	case gatewayCmdGossip:
		s.processImplicitGateway(info)
	default:
		s.Errorf("Unknown command %d from server %v", info.GatewayCmd, routeSrvID)
	}
}

// Sends INFO protocols to the given route connection for each known Gateway.
// These will be processed by the route and delegated to the gateway code to
// imvoke processImplicitGateway.
func (s *Server) sendGatewayConfigsToRoute(route *client) {
	gw := s.gateway
	gw.RLock()
	// Send only to gateways for which we have actual outbound connection to.
	if len(gw.out) == 0 {
		gw.RUnlock()
		return
	}
	// Collect gateway configs for which we have an outbound connection.
	gwCfgsa := [4]*gatewayCfg{}
	gwCfgs := gwCfgsa[:0]
	for _, c := range gw.out {
		c.mu.Lock()
		if c.gw.cfg != nil {
			gwCfgs = append(gwCfgs, c.gw.cfg)
		}
		c.mu.Unlock()
	}
	gw.RUnlock()
	if len(gwCfgs) == 0 {
		return
	}

	// Check forwardNewGatewayToLocalCluster() as to why we set ID this way.
	info := Info{
		ID:         "GW" + s.info.ID,
		GatewayCmd: gatewayCmdGossip,
	}
	for _, cfg := range gwCfgs {
		urls := cfg.getURLsAsStrings()
		if len(urls) > 0 {
			info.Gateway = cfg.Name
			info.GatewayURLs = urls
			b, _ := json.Marshal(&info)
			route.mu.Lock()
			route.sendProto([]byte(fmt.Sprintf(InfoProto, b)), true)
			route.mu.Unlock()
		}
	}
}

// Initiates a gateway connection using the info contained in the INFO protocol.
// If a gateway with the same name is already registered (either because explicitly
// configured, or already implicitly connected), this function will augmment the
// remote URLs with URLs present in the info protocol and return.
// Otherwise, this function will register this remote (to prevent multiple connections
// to the same remote) and call solicitGateway (which will run in a different go-routine).
func (s *Server) processImplicitGateway(info *Info) {
	s.gateway.Lock()
	defer s.gateway.Unlock()
	// Name of the gateway to connect to is the Info.Gateway field.
	gwName := info.Gateway
	// If this is our name, bail.
	if gwName == s.gateway.name {
		return
	}
	// Check if we already have this config, and if so, we are done
	cfg := s.gateway.remotes[gwName]
	if cfg != nil {
		// However, possibly augment the list of URLs with the given
		// info.GatewayURLs content.
		cfg.Lock()
		cfg.addURLs(info.GatewayURLs)
		cfg.Unlock()
		return
	}
	opts := s.getOpts()
	cfg = &gatewayCfg{
		RemoteGatewayOpts: &RemoteGatewayOpts{Name: gwName},
		urls:              make(map[string]*url.URL, len(info.GatewayURLs)),
		implicit:          true,
	}
	if opts.Gateway.TLSConfig != nil {
		cfg.TLSConfig = opts.Gateway.TLSConfig.Clone()
		cfg.TLSTimeout = opts.Gateway.TLSTimeout
	}

	// Since we know we don't have URLs (no config, so just based on what we
	// get from INFO), directly call addURLs(). We don't need locking since
	// we just created that structure and no one else has access to it yet.
	cfg.addURLs(info.GatewayURLs)
	// If there is no URL, we can't proceed.
	if len(cfg.urls) == 0 {
		return
	}
	s.gateway.remotes[gwName] = cfg
	s.startGoRoutine(func() {
		s.solicitGateway(cfg)
		s.grWG.Done()
	})
}

// Returns the number of outbound gateway connections
func (s *Server) numOutboundGateways() int {
	s.gateway.RLock()
	n := len(s.gateway.out)
	s.gateway.RUnlock()
	return n
}

// Returns the number of inbound gateway connections
func (s *Server) numInboundGateways() int {
	s.gateway.RLock()
	n := len(s.gateway.in)
	s.gateway.RUnlock()
	return n
}

// Returns the remoteGateway (if any) that has the given `name`
func (s *Server) getRemoteGateway(name string) *gatewayCfg {
	s.gateway.RLock()
	cfg := s.gateway.remotes[name]
	s.gateway.RUnlock()
	return cfg
}

// Used in tests
func (g *gatewayCfg) bumpConnAttempts() {
	g.Lock()
	g.connAttempts++
	g.Unlock()
}

// Used in tests
func (g *gatewayCfg) getConnAttempts() int {
	g.Lock()
	ca := g.connAttempts
	g.Unlock()
	return ca
}

// Used in tests
func (g *gatewayCfg) resetConnAttempts() {
	g.Lock()
	g.connAttempts = 0
	g.Unlock()
}

// Returns if this remote gateway is implicit or not.
func (g *gatewayCfg) isImplicit() bool {
	g.RLock()
	ii := g.implicit
	g.RUnlock()
	return ii
}

// getURLs returns an array of URLs in random order suitable for
// an iteration to try to connect.
func (g *gatewayCfg) getURLs() []*url.URL {
	g.RLock()
	a := make([]*url.URL, 0, len(g.urls))
	for _, u := range g.urls {
		a = append(a, u)
	}
	g.RUnlock()
	return a
}

// Similar to getURLs but returns the urls as an array of strings.
func (g *gatewayCfg) getURLsAsStrings() []string {
	g.RLock()
	a := make([]string, 0, len(g.urls))
	for _, u := range g.urls {
		a = append(a, u.Host)
	}
	g.RUnlock()
	return a
}

// updateURLs creates the urls map with the content of the config's URLs array
// and the given array that we get from the INFO protocol.
func (g *gatewayCfg) updateURLs(infoURLs []string) {
	g.Lock()
	// Clear the map...
	g.urls = make(map[string]*url.URL, len(g.URLs)+len(infoURLs))
	// Add the urls from the config URLs array.
	for _, u := range g.URLs {
		g.urls[u.Host] = u
	}
	// Then add the ones from the infoURLs array we got.
	g.addURLs(infoURLs)
	g.Unlock()
}

// Saves the hostname of the given URL (if not already done).
// This may be used as the ServerName of the TLSConfig when initiating a
// TLS connection.
// Write lock held on entry.
func (g *gatewayCfg) saveTLSHostname(u *url.URL) {
	if g.TLSConfig != nil && g.tlsName == "" && net.ParseIP(u.Hostname()) == nil {
		g.tlsName = u.Hostname()
	}
}

// add URLs from the given array to the urls map only if not already present.
// remoteGateway write lock is assumed to be held on entry.
// Write lock is held on entry.
func (g *gatewayCfg) addURLs(infoURLs []string) {
	var scheme string
	if g.TLSConfig != nil {
		scheme = "tls"
	} else {
		scheme = "nats"
	}
	for _, iu := range infoURLs {
		if _, present := g.urls[iu]; !present {
			// Urls in Info.GatewayURLs come without scheme. Add it to parse
			// the url (otherwise it fails).
			if u, err := url.Parse(fmt.Sprintf("%s://%s", scheme, iu)); err == nil {
				// Also, if a tlsName has not been set yet and we are dealing
				// with a hostname and not a bare IP, save the hostname.
				g.saveTLSHostname(u)
				// Use u.Host for the key.
				g.urls[u.Host] = u
			}
		}
	}
}

// Adds this URL to the set of Gateway URLs
// Server lock held on entry
func (s *Server) addGatewayURL(urlStr string) {
	s.gateway.Lock()
	s.gateway.URLs[urlStr] = struct{}{}
	s.gateway.Unlock()
}

// Remove this URL from the set of gateway URLs
// Server lock held on entry
func (s *Server) removeGatewayURL(urlStr string) {
	s.gateway.Lock()
	delete(s.gateway.URLs, urlStr)
	s.gateway.Unlock()
}

// This returns the URL of the Gateway listen spec, or empty string
// if the server has no gateway configured.
func (s *Server) getGatewayURL() string {
	s.gateway.RLock()
	url := s.gateway.URL
	s.gateway.RUnlock()
	return url
}

// Returns this server gateway name.
// Same than calling s.gateway.getName()
func (s *Server) getGatewayName() string {
	return s.gateway.getName()
}

// All gateway connections (outbound and inbound) are put in the given map.
func (s *Server) getAllGatewayConnections(conns map[uint64]*client) {
	gw := s.gateway
	gw.RLock()
	for _, c := range gw.out {
		c.mu.Lock()
		cid := c.cid
		c.mu.Unlock()
		conns[cid] = c
	}
	for cid, c := range gw.in {
		conns[cid] = c
	}
	gw.RUnlock()
}

// Register the given gateway connection (*client) in the inbound gateways
// map. The key is the connection ID (like for clients and routes).
func (s *Server) registerInboundGatewayConnection(cid uint64, gwc *client) {
	s.gateway.Lock()
	s.gateway.in[cid] = gwc
	s.gateway.Unlock()
}

// Register the given gateway connection (*client) in the outbound gateways
// map with the given name as the key.
func (s *Server) registerOutboundGatewayConnection(name string, gwc *client) {
	s.gateway.Lock()
	s.gateway.out[name] = gwc
	s.gateway.outo = append(s.gateway.outo, gwc)
	s.gateway.orderOutboundConnectionsLocked()
	s.gateway.Unlock()
}

// Returns the outbound gateway connection (*client) with the given name,
// or nil if not found
func (s *Server) getOutboundGatewayConnection(name string) *client {
	s.gateway.RLock()
	gwc := s.gateway.out[name]
	s.gateway.RUnlock()
	return gwc
}

// Returns all outbound gateway connections in the provided array.
// The order of the gateways is suited for the sending of a message.
// Current ordering is based on individual gateway's RTT value.
func (s *Server) getOutboundGatewayConnections(a *[]*client) {
	s.gateway.RLock()
	for i := 0; i < len(s.gateway.outo); i++ {
		*a = append(*a, s.gateway.outo[i])
	}
	s.gateway.RUnlock()
}

// Orders the array of outbound connections.
// Current ordering is by lowest RTT.
// Gateway write lock is held on entry
func (g *srvGateway) orderOutboundConnectionsLocked() {
	// Order the gateways by lowest RTT
	sort.Slice(g.outo, func(i, j int) bool {
		return g.outo[i].getRTTValue() < g.outo[j].getRTTValue()
	})
}

// Orders the array of outbound connections.
// Current ordering is by lowest RTT.
func (g *srvGateway) orderOutboundConnections() {
	g.Lock()
	g.orderOutboundConnectionsLocked()
	g.Unlock()
}

// Returns all inbound gateway connections in the provided array
func (s *Server) getInboundGatewayConnections(a *[]*client) {
	s.gateway.RLock()
	for _, gwc := range s.gateway.in {
		*a = append(*a, gwc)
	}
	s.gateway.RUnlock()
}

// This is invoked when a gateway connection is closed and the server
// is removing this connection from its state.
func (s *Server) removeRemoteGatewayConnection(c *client) {
	c.mu.Lock()
	cid := c.cid
	isOutbound := c.gw.outbound
	gwName := c.gw.name
	c.mu.Unlock()

	gw := s.gateway
	gw.Lock()
	if isOutbound {
		delete(gw.out, gwName)
		louto := len(gw.outo)
		reorder := false
		for i := 0; i < len(gw.outo); i++ {
			if gw.outo[i] == c {
				// If last, simply remove and no need to reorder
				if i != louto-1 {
					gw.outo[i] = gw.outo[louto-1]
					reorder = true
				}
				gw.outo = gw.outo[:louto-1]
			}
		}
		if reorder {
			gw.orderOutboundConnectionsLocked()
		}
	} else {
		delete(gw.in, cid)
	}
	gw.Unlock()
	s.removeFromTempClients(cid)

	if isOutbound {
		// Update number of totalQSubs for this gateway
		qSubsRemoved := int64(0)
		c.mu.Lock()
		for _, sub := range c.subs {
			if sub.queue != nil {
				qSubsRemoved++
			}
		}
		c.mu.Unlock()
		// Update total count of qsubs in remote gateways.
		atomic.AddInt64(&c.srv.gateway.totalQSubs, -qSubsRemoved)

	} else {
		var subsa [1024]*subscription
		var subs = subsa[:0]

		// For inbound GW connection, if we have subs, those are
		// local subs on "_R_." subjects.
		c.mu.Lock()
		for _, sub := range c.subs {
			subs = append(subs, sub)
		}
		c.mu.Unlock()
		for _, sub := range subs {
			c.removeReplySub(sub)
		}
	}
}

// GatewayAddr returns the net.Addr object for the gateway listener.
func (s *Server) GatewayAddr() *net.TCPAddr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gatewayListener == nil {
		return nil
	}
	return s.gatewayListener.Addr().(*net.TCPAddr)
}

// A- protocol received from the remote after sending messages
// on an account that it has no interest in. Mark this account
// with a "no interest" marker to prevent further messages send.
// <Invoked from outbound connection's readLoop>
func (c *client) processGatewayAccountUnsub(accName string) {
	// Just to indicate activity around "subscriptions" events.
	c.in.subs++
	c.gw.outsim.Store(accName, nil)
}

// A+ protocol received from remote gateway if it had previously
// sent an A-. Clear the "no interest" marker for this account.
// <Invoked from outbound connection's readLoop>
func (c *client) processGatewayAccountSub(accName string) error {
	// Just to indicate activity around "subscriptions" events.
	c.in.subs++
	c.gw.outsim.Delete(accName)
	return nil
}

// RS- protocol received from the remote after sending messages
// on a subject that it has no interest in (but knows about the
// account). Mark this subject with a "no interest" marker to
// prevent further messages being sent.
// If in modeInterestOnly or for a queue sub, remove from
// the sublist if present.
// <Invoked from outbound connection's readLoop>
func (c *client) processGatewayRUnsub(arg []byte) error {
	accName, subject, queue, err := c.parseUnsubProto(arg)
	if err != nil {
		return fmt.Errorf("processGatewaySubjectUnsub %s", err.Error())
	}

	var e *outsie
	var useSl, newe bool

	ei, _ := c.gw.outsim.Load(accName)
	if ei != nil {
		e = ei.(*outsie)
		e.Lock()
		defer e.Unlock()
		// If there is an entry, for plain sub we need
		// to know if we should store the sub
		useSl = queue != nil || e.mode != modeOptimistic
	} else if queue != nil {
		// should not even happen...
		c.Debugf("Received RS- without prior RS+ for subject %q, queue %q", subject, queue)
		return nil
	} else {
		// Plain sub, assume optimistic sends, create entry.
		e = &outsie{ni: make(map[string]struct{}), sl: NewSublist()}
		newe = true
	}
	// This is when a sub or queue sub is supposed to be in
	// the sublist. Look for it and remove.
	if useSl {
		key := arg
		c.mu.Lock()
		defer c.mu.Unlock()
		// m[string()] does not cause mem allocation
		sub, ok := c.subs[string(key)]
		// if RS- for a sub that we don't have, just ignore.
		if !ok {
			return nil
		}
		if e.sl.Remove(sub) == nil {
			delete(c.subs, string(key))
			if queue != nil {
				atomic.AddInt64(&c.srv.gateway.totalQSubs, -1)
			}
			// If last, we can remove the whole entry only
			// when in optimistic mode and there is no element
			// in the `ni` map.
			if e.sl.Count() == 0 && e.mode == modeOptimistic && len(e.ni) == 0 {
				c.gw.outsim.Delete(accName)
			}
		}
	} else {
		e.ni[string(subject)] = struct{}{}
		if newe {
			c.gw.outsim.Store(accName, e)
		}
	}
	return nil
}

// For plain subs, RS+ protocol received from remote gateway if it
// had previously sent a RS-. Clear the "no interest" marker for
// this subject (under this account).
// For queue subs, or if in modeInterestOnly, register interest
// from remote gateway.
// <Invoked from outbound connection's readLoop>
func (c *client) processGatewayRSub(arg []byte) error {
	c.traceInOp("RS+", arg)

	// Indicate activity.
	c.in.subs++

	var (
		queue []byte
		qw    int32
	)

	args := splitArg(arg)
	switch len(args) {
	case 2:
	case 4:
		queue = args[2]
		qw = int32(parseSize(args[3]))
	default:
		return fmt.Errorf("processGatewaySubjectSub Parse Error: '%s'", arg)
	}
	accName := args[0]
	subject := args[1]

	var e *outsie
	var useSl, newe bool

	ei, _ := c.gw.outsim.Load(string(accName))
	// We should always have an existing entry for plain subs because
	// in optimistic mode we would have received RS- first, and
	// in full knowledge, we are receiving RS+ for an account after
	// getting many RS- from the remote..
	if ei != nil {
		e = ei.(*outsie)
		e.Lock()
		defer e.Unlock()
		useSl = queue != nil || e.mode != modeOptimistic
	} else if queue == nil {
		return nil
	} else {
		e = &outsie{ni: make(map[string]struct{}), sl: NewSublist()}
		newe = true
		useSl = true
	}
	if useSl {
		var key []byte
		// We store remote subs by account/subject[/queue].
		// For queue, remove the trailing weight
		if queue != nil {
			key = arg[:len(arg)-len(args[3])-1]
		} else {
			key = arg
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		// If RS+ for a sub that we already have, ignore.
		// (m[string()] does not allocate memory)
		if _, ok := c.subs[string(key)]; ok {
			return nil
		}
		// new subscription. copy subject (and queue) to
		// not reference the underlying possibly big buffer.
		var csubject []byte
		var cqueue []byte
		if queue != nil {
			// make single allocation and use different slices
			// to point to subject and queue name.
			cbuf := make([]byte, len(subject)+1+len(queue))
			copy(cbuf, key[len(accName)+1:])
			csubject = cbuf[:len(subject)]
			cqueue = cbuf[len(subject)+1:]
		} else {
			csubject = make([]byte, len(subject))
			copy(csubject, subject)
		}
		sub := &subscription{client: c, subject: csubject, queue: cqueue, qw: qw}
		// If no error inserting in sublist...
		if e.sl.Insert(sub) == nil {
			c.subs[string(key)] = sub
			if newe {
				c.gw.outsim.Store(string(accName), e)
			}
			if queue != nil {
				atomic.AddInt64(&c.srv.gateway.totalQSubs, 1)
			}
		}
	} else {
		subj := string(subject)
		// If this is an RS+ for a wc subject, then
		// remove from the no interest map all subjects
		// that are a subset of this wc subject.
		if subjectHasWildcard(subj) {
			for k := range e.ni {
				if subjectIsSubsetMatch(k, subj) {
					delete(e.ni, k)
				}
			}
		} else {
			delete(e.ni, subj)
		}
	}
	return nil
}

// Returns true if this gateway has possible interest in the
// given account/subject (which means, it does not have a registered
// no-interest on the account and/or subject) and the sublist result
// for queue subscriptions.
// <Outbound connection: invoked when client message is published,
// so from any client connection's readLoop>
func (c *client) gatewayInterest(acc, subj string) (bool, *SublistResult) {
	ei, accountInMap := c.gw.outsim.Load(acc)
	// If there is an entry for this account and ei is nil,
	// it means that the remote is not interested at all in
	// this account and we could not possibly have queue subs.
	if accountInMap && ei == nil {
		return false, nil
	}
	// Assume interest if account not in map.
	psi := !accountInMap
	var r *SublistResult
	if accountInMap {
		// If in map, check for subs interest with sublist.
		e := ei.(*outsie)
		r = e.sl.Match(subj)
		// If there is plain subs returned, we don't have to
		// check if we should use the no-interest map because
		// it means that we are in modeInterestOnly.
		// Only if there is nothing returned for r.psubs that
		// we need to check.
		if len(r.psubs) > 0 {
			psi = true
		} else {
			e.RLock()
			// We may be in transition to modeInterestOnly
			// but until e.ni is nil, use it to know if we
			// should suppress interest or not.
			if e.ni != nil {
				if _, inMap := e.ni[subj]; !inMap {
					psi = true
				}
			}
			e.RUnlock()
		}
	}
	return psi, r
}

// This is invoked when an account is registered. We check if we did send
// to remote gateways a "no interest" in the past when receiving messages.
// If we did, we send to the remote gateway an A+ protocol (see
// processGatewayAccountSub()).
// <Invoked from outbound connection's readLoop>
func (s *Server) endAccountNoInterestForGateways(accName string) {
	gwsa := [4]*client{}
	gws := gwsa[:0]
	s.getInboundGatewayConnections(&gws)
	if len(gws) == 0 {
		return
	}
	var protoa [256]byte
	var proto []byte
	for _, c := range gws {
		c.mu.Lock()
		// If value in map, it means we sent an A- and need
		// to clear and send A+ now.
		if _, inMap := c.gw.insim[accName]; inMap {
			delete(c.gw.insim, accName)
			if proto == nil {
				proto = protoa[:0]
				proto = append(proto, aSubBytes...)
				proto = append(proto, accName...)
				proto = append(proto, CR_LF...)
			}
			if c.trace {
				c.traceOutOp("", proto[:len(proto)-LEN_CR_LF])
			}
			c.sendProto(proto, true)
		}
		c.mu.Unlock()
	}
}

// This is invoked when registering (or unregistering) the first
// (or last) subscription on a given account/subject. For each
// GWs inbound connections, we will check if we need to send
// the protocol. In optimistic mode we would send an RS+ only
// if we had previously sent an RS-. If we are in the send-all-subs
// mode then the protocol is always sent.
// <Invoked from outbound connection's readLoop>
func (s *Server) maybeSendSubOrUnsubToGateways(accName string, sub *subscription, added bool) {
	if sub.queue != nil {
		return
	}
	gwsa := [4]*client{}
	gws := gwsa[:0]
	s.getInboundGatewayConnections(&gws)
	if len(gws) == 0 {
		return
	}
	var (
		protoa  [512]byte
		proto   []byte
		subject = string(sub.subject)
		hasWc   = subjectHasWildcard(subject)
	)
	for _, c := range gws {
		sendProto := false
		c.mu.Lock()
		e := c.gw.insim[accName]
		if e != nil {
			// If there is a map, need to check if we had sent no-interest.
			if e.ni != nil {
				// For wildcard subjects, we will remove from our no-interest
				// map, all subjects that are a subset of this wc subject, but we
				// still send the wc subject and let the remote do its own cleanup.
				if hasWc {
					for enis := range e.ni {
						if subjectIsSubsetMatch(enis, subject) {
							delete(e.ni, enis)
							sendProto = true
						}
					}
				} else if _, noInterest := e.ni[subject]; noInterest {
					delete(e.ni, subject)
					sendProto = true
				}
			} else if e.mode == modeInterestOnly {
				// We are in the mode where we always send RS+/- protocols.
				sendProto = true
			}
		}
		if sendProto {
			if proto == nil {
				proto = protoa[:0]
				if added {
					proto = append(proto, rSubBytes...)
				} else {
					proto = append(proto, rUnsubBytes...)
				}
				proto = append(proto, accName...)
				proto = append(proto, ' ')
				proto = append(proto, sub.subject...)
				proto = append(proto, CR_LF...)
			}
			if c.trace {
				c.traceOutOp("", proto[:len(proto)-LEN_CR_LF])
			}
			c.sendProto(proto, true)
		}
		c.mu.Unlock()
	}
}

// This is invoked when the first (or last) queue subscription on a
// given subject/group is registered (or unregistered). Sent to all
// inbound gateways.
func (s *Server) sendQueueSubOrUnsubToGateways(accName string, qsub *subscription, added bool) {
	if qsub.queue == nil {
		return
	}

	gwsa := [4]*client{}
	gws := gwsa[:0]
	s.getInboundGatewayConnections(&gws)
	if len(gws) == 0 {
		return
	}

	var protoa [512]byte
	var proto []byte
	for _, c := range gws {
		if proto == nil {
			proto = protoa[:0]
			if added {
				proto = append(proto, rSubBytes...)
			} else {
				proto = append(proto, rUnsubBytes...)
			}
			proto = append(proto, accName...)
			proto = append(proto, ' ')
			proto = append(proto, qsub.subject...)
			proto = append(proto, ' ')
			proto = append(proto, qsub.queue...)
			if added {
				// For now, just use 1 for the weight
				proto = append(proto, ' ', '1')
			}
			proto = append(proto, CR_LF...)
		}
		c.mu.Lock()
		if c.trace {
			c.traceOutOp("", proto[:len(proto)-LEN_CR_LF])
		}
		c.sendProto(proto, true)
		c.mu.Unlock()
	}
}

// This is invoked when a (queue) subscription is added/removed locally
// or in our cluster. We use ref counting to know when to update
// the inbound gateways.
// <Invoked from client or route connection's readLoop or when such
// connection is closed>
func (s *Server) gatewayUpdateSubInterest(accName string, sub *subscription, change int32) {
	var (
		keya  [1024]byte
		key   = keya[:0]
		entry *sitally
		isNew bool
	)

	s.gateway.pasi.Lock()
	defer s.gateway.pasi.Unlock()

	accMap := s.gateway.pasi.m

	// First see if we have the account
	st := accMap[accName]
	if st == nil {
		// Ignore remove of something we don't have
		if change < 0 {
			return
		}
		st = make(map[string]*sitally)
		accMap[accName] = st
		isNew = true
	}
	// Lookup: build the key as subject[+' '+queue]
	key = append(key, sub.subject...)
	if sub.queue != nil {
		key = append(key, ' ')
		key = append(key, sub.queue...)
	}
	if !isNew {
		entry = st[string(key)]
	}
	first := false
	last := false
	if entry == nil {
		// Ignore remove of something we don't have
		if change < 0 {
			return
		}
		entry = &sitally{n: 1, q: sub.queue != nil}
		st[string(key)] = entry
		first = true
	} else {
		entry.n += change
		if entry.n <= 0 {
			delete(st, string(key))
			last = true
		}
	}
	if first || last {
		if entry.q {
			s.sendQueueSubOrUnsubToGateways(accName, sub, first)
		} else {
			s.maybeSendSubOrUnsubToGateways(accName, sub, first)
		}
	}
}

// Invoked by a PUB's connection to send a reply message on _R_ directly
// to the gateway connection.
func (c *client) sendReplyMsgDirectToGateway(acc *Account, sub *subscription, msg []byte) {
	// The sub.client references the inbound connection, so we need to
	// swap to the outbound connection here.
	inbound := sub.client
	outbound := c.srv.getOutboundGatewayConnection(inbound.gw.name)
	sub.client = outbound

	mh := c.msgb[:msgHeadProtoLen]
	mh = append(mh, acc.Name...)
	mh = append(mh, ' ')
	mh = append(mh, sub.subject...)
	mh = append(mh, ' ')
	mh = append(mh, c.pa.szb...)
	mh = append(mh, CR_LF...)
	c.deliverMsg(sub, mh, msg)
	// Cleanup. Since the sub is stored in the inbound, use that to
	// call this function.
	inbound.removeReplySub(sub)
}

// May send a message to all outbound gateways. It is possible
// that message is not sent to a given gateway if for instance
// it is known that this gateway has no interest in account or
// subject, etc..
// <Invoked from any client connection's readLoop>
func (c *client) sendMsgToGateways(acc *Account, msg, subject, reply []byte, qgroups [][]byte) {
	gwsa := [4]*client{}
	gws := gwsa[:0]
	// This is in fast path, so avoid calling function when possible.
	// Get the outbound connections in place instead of calling
	// getOutboundGatewayConnections().
	gw := c.srv.gateway
	gw.RLock()
	for i := 0; i < len(gw.outo); i++ {
		gws = append(gws, gw.outo[i])
	}
	gw.RUnlock()
	if len(gws) == 0 {
		return
	}
	var (
		subj    = string(subject)
		queuesa = [512]byte{}
		queues  = queuesa[:0]
		accName = acc.Name
	)
	for i := 0; i < len(gws); i++ {
		gwc := gws[i]
		// Plain sub interest and queue sub results for this account/subject
		psi, qr := gwc.gatewayInterest(accName, subj)
		if !psi && qr == nil {
			continue
		}
		queues = queuesa[:0]
		if qr != nil {
			for i := 0; i < len(qr.qsubs); i++ {
				qsubs := qr.qsubs[i]
				if len(qsubs) > 0 {
					queue := qsubs[0].queue
					add := true
					for _, qn := range qgroups {
						if bytes.Equal(queue, qn) {
							add = false
							break
						}
					}
					if add {
						qgroups = append(qgroups, queue)
						queues = append(queues, queue...)
						queues = append(queues, ' ')
					}
				}
			}
		}
		if !psi && len(queues) == 0 {
			continue
		}
		mh := c.msgb[:msgHeadProtoLen]
		mh = append(mh, accName...)
		mh = append(mh, ' ')
		mh = append(mh, subject...)
		mh = append(mh, ' ')
		if len(queues) > 0 {
			if reply != nil {
				mh = append(mh, "+ "...) // Signal that there is a reply.
				mh = append(mh, reply...)
				mh = append(mh, ' ')
			} else {
				mh = append(mh, "| "...) // Only queues
			}
			mh = append(mh, queues...)
		} else if reply != nil {
			mh = append(mh, reply...)
			mh = append(mh, ' ')
		}
		mh = append(mh, c.pa.szb...)
		mh = append(mh, CR_LF...)
		sub := subscription{client: gwc, subject: c.pa.subject}
		c.deliverMsg(&sub, mh, msg)
	}
}

// Process a message coming from a remote gateway. Send to any sub/qsub
// in our cluster that is matching. When receiving a message for an
// account or subject for which there is no interest in this cluster
// an A-/RS- protocol may be send back.
// <Invoked from inbound connection's readLoop>
func (c *client) processInboundGatewayMsg(msg []byte) {
	// Update statistics
	c.in.msgs++
	// The msg includes the CR_LF, so pull back out for accounting.
	c.in.bytes += len(msg) - LEN_CR_LF

	if c.trace {
		c.traceMsg(msg)
	}

	if c.opts.Verbose {
		c.sendOK()
	}

	// Mostly under testing scenarios.
	if c.srv == nil {
		return
	}

	acc, r := c.getAccAndResultFromCache()
	if acc == nil {
		c.Debugf("Unknown account %q for routed message on subject: %q", c.pa.account, c.pa.subject)
		// Send A- only once...
		c.mu.Lock()
		if _, sent := c.gw.insim[string(c.pa.account)]; !sent {
			// Add a nil value to indicate that we have sent an A-
			// so that we know to send A+ when/if account gets registered.
			c.gw.insim[string(c.pa.account)] = nil
			var protoa [256]byte
			proto := protoa[:0]
			proto = append(proto, aUnsubBytes...)
			proto = append(proto, c.pa.account...)
			if c.trace {
				c.traceOutOp("", proto)
			}
			proto = append(proto, CR_LF...)
			c.sendProto(proto, true)
		}
		c.mu.Unlock()
		return
	}

	// Check to see if we need to map/route to another account.
	if acc.imports.services != nil {
		c.checkForImportServices(acc, msg)
	}

	// If there is no interest on plain subs, possibly send an RS-,
	// even if there is qsubs interest.
	if len(r.psubs) == 0 {
		sendProto := false
		c.mu.Lock()
		// Send an RS- protocol if not already done and only if
		// not in the send-all-subs mode.
		e := c.gw.insim[string(c.pa.account)]
		if e == nil {
			e = &insie{ni: make(map[string]struct{})}
			e.ni[string(c.pa.subject)] = struct{}{}
			c.gw.insim[string(c.pa.account)] = e
			sendProto = true
		} else if e.ni != nil {
			// If we are not in send-all-subs mode, check if we
			// have already sent an RS-
			if _, alreadySent := e.ni[string(c.pa.subject)]; !alreadySent {
				// TODO(ik): pick some threshold as to when
				// we need to switch mode
				if len(e.ni) > gatewayMaxRUnsubBeforeSwitch {
					// If too many RS-, switch to all-subs-mode.
					c.gatewaySwitchAccountToSendAllSubs(e)
				} else {
					e.ni[string(c.pa.subject)] = struct{}{}
					sendProto = true
				}
			}
		}
		if sendProto {
			var (
				protoa = [512]byte{}
				proto  = protoa[:0]
			)
			proto = append(proto, rUnsubBytes...)
			proto = append(proto, c.pa.account...)
			proto = append(proto, ' ')
			proto = append(proto, c.pa.subject...)
			if c.trace {
				c.traceOutOp("", proto)
			}
			proto = append(proto, CR_LF...)
			c.sendProto(proto, true)
		}
		c.mu.Unlock()
		if len(r.qsubs) == 0 || len(c.pa.queues) == 0 {
			return
		}
	}

	// Check to see if we have a routed message with a service reply.
	if isServiceReply(c.pa.reply) && acc != nil {
		// Need to add a sub here for local interest to send a response back
		// to the originating server/requestor where it will be re-mapped.
		sid := make([]byte, 0, len(acc.Name)+len(c.pa.reply)+1)
		sid = append(sid, acc.Name...)
		sid = append(sid, ' ')
		sid = append(sid, c.pa.reply...)
		// Copy off the reply since otherwise we are referencing a buffer that will be reused.
		reply := make([]byte, len(c.pa.reply))
		copy(reply, c.pa.reply)
		sub := &subscription{client: c, subject: reply, sid: sid, max: 1}
		if err := acc.sl.Insert(sub); err != nil {
			c.Errorf("Could not insert subscription: %v", err)
		} else {
			ttl := acc.AutoExpireTTL()
			c.mu.Lock()
			c.subs[string(sid)] = sub
			c.addReplySubTimeout(acc, sub, ttl)
			c.mu.Unlock()
		}
	}

	c.processMsgResults(acc, r, msg, c.pa.subject, c.pa.reply, nil)
}

// Indicates that the remote which we are sending messages to
// has decided to send us all its subs interest so that we
// stop doing optimistic sends.
// <Invoked from outbound connection's readLoop>
func (c *client) gatewayAllSubsReceiveStart(info *Info) {
	account := getAccountFromGatewayCommand(c, info, "start")
	if account == "" {
		return
	}
	// Since the remote would send us this start command
	// only after sending us too many RS- for this account,
	// we should always have an entry here.
	// TODO(ik): Should we close connection with protocol violation
	// error if that happens?
	ei, _ := c.gw.outsim.Load(account)
	if ei != nil {
		e := ei.(*outsie)
		// Would not even need locking here since this is
		// checked only from this go routine, but it's a
		// one-time event, so...
		e.Lock()
		e.mode = modeTransitioning
		e.Unlock()
	}
}

// Indicates that the remote has finished sending all its
// subscriptions and we should now not send unless we know
// there is explicit interest.
// <Invoked from outbound connection's readLoop>
func (c *client) gatewayAllSubsReceiveComplete(info *Info) {
	account := getAccountFromGatewayCommand(c, info, "complete")
	if account == "" {
		return
	}
	// Done receiving all subs from remote. Set the `ni`
	// map to nil so that gatewayInterest() no longer
	// uses it.
	ei, _ := c.gw.outsim.Load(string(account))
	if ei != nil {
		e := ei.(*outsie)
		// Needs locking here since `ni` is checked by
		// many go-routines calling gatewayInterest()
		e.Lock()
		e.ni = nil
		e.mode = modeInterestOnly
		e.Unlock()
	}
}

// small helper to get the account name from the INFO command.
func getAccountFromGatewayCommand(c *client, info *Info, cmd string) string {
	if info.GatewayCmdPayload == nil {
		c.sendErrAndErr(fmt.Sprintf("Account absent from receive-all-subscriptions-%s command", cmd))
		c.closeConnection(ProtocolViolation)
		return ""
	}
	return string(info.GatewayCmdPayload)
}

// Switch to send-all-subs mode for the given gateway and account.
// This is invoked when processing an inbound message and we
// reach a point where we had to send a lot of RS- for this
// account. We will send an INFO protocol to indicate that we
// start sending all our subs (for this account), followed by
// all subs (RS+) and finally an INFO to indicate the end of it.
// The remote will then send messages only if it finds explicit
// interest in the sublist created based on all RS+ that we just
// sent.
// The client's lock is held on entry.
// <Invoked from inbound connection's readLoop>
func (c *client) gatewaySwitchAccountToSendAllSubs(e *insie) {
	// Set this map to nil so that the no-interest is
	// no longer checked.
	e.ni = nil
	// Capture this since we are passing it to a go-routine.
	account := string(c.pa.account)
	s := c.srv

	// Function that will create an INFO protocol
	// and set proper command.
	sendCmd := func(cmd byte, useLock bool) {
		// Use bare server info and simply set the
		// gateway name and command
		info := Info{
			Gateway:           s.getGatewayName(),
			GatewayCmd:        cmd,
			GatewayCmdPayload: []byte(account),
		}
		b, _ := json.Marshal(&info)
		infoJSON := []byte(fmt.Sprintf(InfoProto, b))
		if useLock {
			c.mu.Lock()
		}
		c.sendProto(infoJSON, true)
		if useLock {
			c.mu.Unlock()
		}
	}
	// Send the start command. When remote receives this,
	// it may continue to send optimistic messages, but
	// it will start to register RS+/RS- in sublist instead
	// of noInterest map.
	sendCmd(gatewayCmdAllSubsStart, false)

	// Execute this in separate go-routine as to not block
	// the readLoop (which may cause the otherside to close
	// the connection due to slow consumer)
	s.startGoRoutine(func() {
		defer s.grWG.Done()

		s.sendAccountSubsToGateway(c, []byte(account))
		// Send the complete command. When the remote receives
		// this, it will not send a message unless it has a
		// matching sub from us.
		sendCmd(gatewayCmdAllSubsComplete, true)
	})
}
