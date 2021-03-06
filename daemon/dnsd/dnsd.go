package dnsd

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/HouzuoGuo/laitos/inet"
	"github.com/HouzuoGuo/laitos/misc"
	"net"
	"strings"
	"sync"
	"time"
)

const (
	RateLimitIntervalSec       = 10   // Rate limit is calculated at 10 seconds interval
	IOTimeoutSec               = 60   // IO timeout for both read and write operations
	MaxPacketSize              = 9038 // Maximum acceptable UDP packet size
	NumQueueRatio              = 10   // Upon initialisation, create (PerIPLimit/NumQueueRatio) number of queues to handle queries.
	BlacklistUpdateIntervalSec = 7200 // Update ad-server blacklist at this interval
	MinNameQuerySize           = 14   // If a query packet is shorter than this length, it cannot possibly be a name query.
	PublicIPRefreshIntervalSec = 900  // PublicIPRefreshIntervalSec is how often the program places its latest public IP address into array of IPs that may query the server.
	MVPSLicense                = `Disclaimer: this file is free to use for personal use only. Furthermore it is NOT permitted to ` +
		`copy any of the contents or host on any other site without permission or meeting the full criteria of the below license ` +
		` terms. This work is licensed under the Creative Commons Attribution-NonCommercial-ShareAlike License. ` +
		` http://creativecommons.org/licenses/by-nc-sa/4.0/ License info for commercial purposes contact Winhelp2002`
)

// A query to forward to DNS forwarder via DNS.
type UDPQuery struct {
	MyServer    *net.UDPConn
	ClientAddr  *net.UDPAddr
	QueryPacket []byte
}

// A query to forward to DNS forwarder via TCP.
type TCPForwarderQuery struct {
	MyServer    *net.Conn
	QueryPacket []byte
}

// A DNS forwarder daemon that selectively refuse to answer certain A record requests made against advertisement servers.
type Daemon struct {
	Address              string   `json:"Address"`              // Network address for both TCP and UDP to listen to, e.g. 0.0.0.0 for all network interfaces.
	AllowQueryIPPrefixes []string `json:"AllowQueryIPPrefixes"` // AllowQueryIPPrefixes are the string prefixes in IPv4 and IPv6 client addresses that are allowed to query the DNS server.
	PerIPLimit           int      `json:"PerIPLimit"`           // How many times in 10 seconds interval an IP may send DNS request

	UDPPort      int      `json:"UDPPort"`       // UDP port to listen on
	UDPForwarder []string `json:"UDPForwarders"` // Forward UDP DNS queries to these address (IP:Port)
	TCPPort      int      `json:"TCPPort"`       // TCP port to listen on
	TCPForwarder []string `json:"TCPForwarders"` // Forward TCP DNS queries to these addresses (IP:Port)

	tcpListener       net.Listener     // Once TCP daemon is started, this is its listener.
	udpForwardConn    []net.Conn       // UDP connections made toward forwarder
	udpForwarderQueue []chan *UDPQuery // Processing queues that handle UDP forward queries
	udpBlackHoleQueue []chan *UDPQuery // Processing queues that handle UDP black-list answers
	udpListener       *net.UDPConn     // Once UDP daemon is started, this is its listener.

	blackListMutex       *sync.Mutex         // Protect against concurrent access to black list
	blackList            map[string]struct{} // Do not answer to type A queries made toward these domains
	allowQueryMutex      *sync.Mutex         // allowQueryMutex guards against concurrent access to AllowQueryIPPrefixes.
	allowQueryLastUpdate int64               // allowQueryLastUpdate is the Unix timestamp of the very latest automatic placement of computer's public IP into the array of AllowQueryIPPrefixes.
	rateLimit            *misc.RateLimit     // Rate limit counter
	logger               misc.Logger
}

// Check configuration and initialise internal states.
func (daemon *Daemon) Initialise() error {
	daemon.logger = misc.Logger{ComponentName: "DNSD", ComponentID: fmt.Sprintf("%s:%d&%d", daemon.Address, daemon.TCPPort, daemon.UDPPort)}
	if daemon.Address == "" {
		return errors.New("DNSD.Initialise: listen address must not be empty")
	}
	if daemon.UDPPort < 1 && daemon.TCPPort < 1 {
		return errors.New("DNSD.Initialise: either or both TCP and UDP ports must be specified and be greater than 0")
	}
	if (daemon.UDPForwarder == nil || len(daemon.UDPForwarder) == 0) && (daemon.TCPForwarder == nil || len(daemon.TCPForwarder) == 0) {
		return errors.New("DNSD.Initialise: there must be at least one UDP or TCP forwarder address")
	}
	if daemon.PerIPLimit < 10 {
		return errors.New("DNSD.Initialise: PerIPLimit must be greater than 9")
	}
	if len(daemon.AllowQueryIPPrefixes) == 0 {
		return errors.New("DNSD.Initialise: allowable IP prefixes list must not be empty")
	}
	for _, prefix := range daemon.AllowQueryIPPrefixes {
		if prefix == "" {
			return errors.New("DNSD.Initialise: any allowable IP prefixes must not be empty string")
		}
	}
	// Always allow localhost to query via both IPv4 and IPv6
	daemon.AllowQueryIPPrefixes = append(daemon.AllowQueryIPPrefixes, "127.", "::1")

	daemon.allowQueryMutex = new(sync.Mutex)
	daemon.blackListMutex = new(sync.Mutex)
	daemon.blackList = make(map[string]struct{})

	daemon.rateLimit = &misc.RateLimit{
		MaxCount: daemon.PerIPLimit,
		UnitSecs: RateLimitIntervalSec,
		Logger:   daemon.logger,
	}
	daemon.rateLimit.Initialise()
	// Create a number of forwarder queues to handle incoming UDP DNS queries
	// Keep in mind, TCP queries are not handled by queues.
	if daemon.UDPPort > 0 {
		numQueues := daemon.PerIPLimit / NumQueueRatio
		// At very least, each forwarder address has to get a queue.
		if numQueues < len(daemon.UDPForwarder) {
			numQueues = len(daemon.UDPForwarder)
		}
		daemon.udpForwardConn = make([]net.Conn, numQueues)
		daemon.udpForwarderQueue = make([]chan *UDPQuery, numQueues)
		daemon.udpBlackHoleQueue = make([]chan *UDPQuery, numQueues)
		for i := 0; i < numQueues; i++ {
			/*
				Each queue is connected to a different forwarder.
				When a DNS query comes in, it is assigned a random forwarder to be processed.
			*/
			forwarderAddr, err := net.ResolveUDPAddr("udp", daemon.UDPForwarder[i%len(daemon.UDPForwarder)])
			if err != nil {
				return fmt.Errorf("DNSD.Initialise: failed to resolve UDP address - %v", err)
			}
			forwarderConn, err := net.DialTimeout("udp", forwarderAddr.String(), IOTimeoutSec*time.Second)
			if err != nil {
				return fmt.Errorf("DNSD.Initialise: failed to connect to UDP forwarder - %v", err)
			}
			daemon.udpForwardConn[i] = forwarderConn
			daemon.udpForwarderQueue[i] = make(chan *UDPQuery, 16) // there really is no need for a deeper queue
			daemon.udpBlackHoleQueue[i] = make(chan *UDPQuery, 4)  // there is also no need for a deeper queue here
		}
	}

	// Always allow server to query itself via public IP
	daemon.allowMyPublicIP()
	return nil
}

// allowMyPublicIP places the computer's public IP address into the array of IPs allowed to query the server.
func (daemon *Daemon) allowMyPublicIP() {
	if daemon.allowQueryLastUpdate+PublicIPRefreshIntervalSec >= time.Now().Unix() {
		return
	}
	daemon.allowQueryMutex.Lock()
	defer daemon.allowQueryMutex.Unlock()
	defer func() {
		// This routine runs periodically no matter it succeeded or failed in retrieving latest public IP
		daemon.allowQueryLastUpdate = time.Now().Unix()
	}()
	latestIP := inet.GetPublicIP()
	if latestIP == "" {
		// Not a fatal error if IP cannot be determined
		daemon.logger.Warningf("allowMyPublicIP", "", nil, "unable to determine public IP address, the computer will not be able to send query to itself.")
		return
	}
	foundMyIP := false
	for _, allowedIP := range daemon.AllowQueryIPPrefixes {
		if allowedIP == latestIP {
			foundMyIP = true
			break
		}
	}
	if !foundMyIP {
		// Place latest IP into the array, but do not erase the old IP entries.
		daemon.AllowQueryIPPrefixes = append(daemon.AllowQueryIPPrefixes, latestIP)
		daemon.logger.Printf("allowMyPublicIP", "", nil, "the latest public IP address %s of this computer is now allowed to query", latestIP)
	}
}

// checkAllowClientIP returns true only if the input IP address is among the allowed addresses.
func (daemon *Daemon) checkAllowClientIP(clientIP string) bool {
	// At regular time interval, make sure that the latest public IP is allowed to query.
	daemon.allowMyPublicIP()

	daemon.allowQueryMutex.Lock()
	defer daemon.allowQueryMutex.Unlock()
	for _, prefix := range daemon.AllowQueryIPPrefixes {
		if strings.HasPrefix(clientIP, prefix) {
			return true
		}
	}
	return false
}

// Download ad-servers list from pgl.yoyo.org and return those domain names.
func (daemon *Daemon) GetAdBlacklistPGL() ([]string, error) {
	yoyo := "https://pgl.yoyo.org/adservers/serverlist.php?hostformat=nohtml&showintro=0&mimetype=plaintext"
	resp, err := inet.DoHTTP(inet.HTTPRequest{TimeoutSec: 30}, yoyo)
	if err != nil {
		return nil, err
	}
	if statusErr := resp.Non2xxToError(); statusErr != nil {
		return nil, statusErr
	}
	lines := strings.Split(string(resp.Body), "\n")
	if len(lines) < 100 {
		return nil, fmt.Errorf("DNSD.GetAdBlacklistPGL: PGL's ad-server list is suspiciously short at only %d lines", len(lines))
	}
	names := make([]string, 0, len(lines))
	for _, line := range lines {
		names = append(names, strings.TrimSpace(line))
	}
	return names, nil
}

// Download ad-servers list from winhelp2002.mvps.org and return those domain names.
func (daemon *Daemon) GetAdBlacklistMVPS() ([]string, error) {
	yoyo := "http://winhelp2002.mvps.org/hosts.txt"
	resp, err := inet.DoHTTP(inet.HTTPRequest{TimeoutSec: 30}, yoyo)
	if err != nil {
		return nil, err
	}
	if statusErr := resp.Non2xxToError(); statusErr != nil {
		return nil, statusErr
	}
	// Collect host names from the hosts file content
	names := make([]string, 0, 16384)
	for _, line := range strings.Split(string(resp.Body), "\n") {
		indexZero := strings.Index(line, "0.0.0.0")
		nameEnd := strings.IndexRune(line, '#')
		if indexZero == -1 {
			// Skip lines that do not have a host name
			continue
		}
		if nameEnd == -1 {
			nameEnd = len(line)
		}
		nameBegin := indexZero + len("0.0.0.0")
		if nameBegin >= nameEnd {
			// The line looks like # this is a comment 0.0.0.0
			continue
		}
		names = append(names, strings.TrimSpace(line[nameBegin:nameEnd]))
	}
	if len(names) < 100 {
		return nil, fmt.Errorf("DNSD.GetAdBlacklistMVPS: MVPS' ad-server list is suspiciously short at only %d lines", len(names))
	}
	return names, nil
}

var StandardResponseNoError = []byte{129, 128} // DNS response packet flag - standard response, no indication of error.

//                            Domain     A    IN      TTL 1466  IPv4     0.0.0.0
var BlackHoleAnswer = []byte{192, 12, 0, 1, 0, 1, 0, 0, 5, 186, 0, 4, 0, 0, 0, 0} // DNS answer 0.0.0.0

// Create a DNS response packet without prefix length bytes, that points incoming query to 0.0.0.0.
func RespondWith0(queryNoLength []byte) []byte {
	if queryNoLength == nil || len(queryNoLength) < MinNameQuerySize {
		return []byte{}
	}
	answerPacket := make([]byte, 2+2+len(queryNoLength)-4+len(BlackHoleAnswer))
	// Match transaction ID of original query
	answerPacket[0] = queryNoLength[0]
	answerPacket[1] = queryNoLength[1]
	// 0x8180 - response is a standard query response, without indication of error.
	copy(answerPacket[2:4], StandardResponseNoError)
	// Copy of original query structure
	copy(answerPacket[4:], queryNoLength[4:])
	// There is exactly one answer RR
	answerPacket[6] = 0
	answerPacket[7] = 1
	// Answer 0.0.0.0 to the query
	copy(answerPacket[len(answerPacket)-len(BlackHoleAnswer):], BlackHoleAnswer)
	// Finally, respond!
	return answerPacket
}

/*
Extract domain name asked by the DNS query. Return the domain name itself, and then with leading components removed.
E.g. for a query packet that asks for "a.b.github.com", the function returns:
- a.b.github.com
- b.github.com
- github.com
*/
func ExtractDomainName(packet []byte) (ret []string) {
	ret = make([]string, 0, 8)
	if packet == nil || len(packet) < MinNameQuerySize {
		return
	}
	indexTypeAClassIN := bytes.Index(packet[13:], []byte{0, 1, 0, 1})
	if indexTypeAClassIN < 1 {
		return
	}
	indexTypeAClassIN += 13
	// The byte right before Type-A Class-IN is an empty byte to be discarded
	domainNameBytes := make([]byte, indexTypeAClassIN-13-1)
	copy(domainNameBytes, packet[13:indexTypeAClassIN-1])
	/*
		Restore full-stops of the domain name portion so that it can be checked against black list.
		Not sure why those byte ranges show up in place of full-stops, probably due to some RFCs.
	*/
	for i, b := range domainNameBytes {
		if b <= 44 || b >= 58 && b <= 64 || b >= 91 && b <= 96 {
			domainNameBytes[i] = '.'
		}
	}
	// First return value is domain name unchanged
	domainName := string(domainNameBytes)
	if len(domainName) > 1024 {
		// Domain name is unrealistically long
		return
	}
	ret = append(ret, domainName)
	// Append more of the same domain name, each with leading component removed.
	for {
		index := strings.IndexRune(domainName, '.')
		if index < 1 || index == len(domainName)-1 {
			break
		}
		domainName = domainName[index+1:]
		ret = append(ret, domainName)
	}
	return
}

func (daemon *Daemon) UpdatedAdBlockLists() {
	pglEntries, pglErr := daemon.GetAdBlacklistPGL()
	if pglErr == nil {
		daemon.logger.Printf("GetAdBlacklistPGL", "", nil, "successfully retrieved ad-blacklist with %d entries", len(pglEntries))
	} else {
		daemon.logger.Warningf("GetAdBlacklistPGL", "", pglErr, "failed to update ad-blacklist")
	}
	mvpsEntries, mvpsErr := daemon.GetAdBlacklistMVPS()
	if mvpsErr == nil {
		daemon.logger.Printf("GetAdBlacklistMVPS", "", nil, "successfully retrieved ad-blacklist with %d entries", len(mvpsEntries))
		daemon.logger.Printf("GetAdBlacklistMVPS", "", nil, "Please comply with the following liences for your usage of http://winhelp2002.mvps.org/hosts.txt: %s", MVPSLicense)
	} else {
		daemon.logger.Warningf("GetAdBlacklistMVPS", "", mvpsErr, "failed to update ad-blacklist")
	}
	daemon.blackListMutex.Lock()
	daemon.blackList = make(map[string]struct{})
	if pglErr == nil {
		for _, name := range pglEntries {
			daemon.blackList[name] = struct{}{}
		}
	}
	if mvpsErr == nil {
		for _, name := range mvpsEntries {
			daemon.blackList[name] = struct{}{}
		}
	}
	daemon.blackListMutex.Unlock()
	daemon.logger.Printf("UpdatedAdBlockLists", "", nil, "ad-blacklist now has %d entries", len(daemon.blackList))
}

/*
You may call this function only after having called Initialise()!
Start DNS daemon on configured TCP and UDP ports. Block caller until both listeners are told to stop.
If either TCP or UDP port fails to listen, all listeners are closed and an error is returned.
*/
func (daemon *Daemon) StartAndBlock() error {
	// Keep updating ad-block black list in background
	stopAdBlockUpdater := make(chan bool, 1)
	go func() {
		daemon.UpdatedAdBlockLists()
		for {
			select {
			case <-stopAdBlockUpdater:
				return
			case <-time.After(BlacklistUpdateIntervalSec * time.Second):
				daemon.UpdatedAdBlockLists()
			}
		}
	}()
	numListeners := 0
	errChan := make(chan error, 2)
	if daemon.UDPPort != 0 {
		numListeners++
		go func() {
			err := daemon.StartAndBlockUDP()
			errChan <- err
			stopAdBlockUpdater <- true
		}()
	}
	if daemon.TCPPort != 0 {
		numListeners++
		go func() {
			err := daemon.StartAndBlockTCP()
			errChan <- err
			stopAdBlockUpdater <- true
		}()
	}
	for i := 0; i < numListeners; i++ {
		if err := <-errChan; err != nil {
			daemon.Stop()
			return err
		}
	}
	return nil
}

// Close all of open TCP and UDP listeners so that they will cease processing incoming connections.
func (daemon *Daemon) Stop() {
	if listener := daemon.tcpListener; listener != nil {
		if err := listener.Close(); err != nil {
			daemon.logger.Warningf("Stop", "", err, "failed to close TCP listener")
		}
	}
	if listener := daemon.udpListener; listener != nil {
		if err := listener.Close(); err != nil {
			daemon.logger.Warningf("Stop", "", err, "failed to close UDP listener")
		}
	}
}

// Return true if any of the input domain names is black listed.
func (daemon *Daemon) NamesAreBlackListed(names []string) bool {
	daemon.blackListMutex.Lock()
	defer daemon.blackListMutex.Unlock()
	var blacklisted bool
	for _, name := range names {
		_, blacklisted = daemon.blackList[name]
		if blacklisted {
			return true
		}
	}
	return false
}

var githubComTCPQuery, githubComUDPQuery []byte // Sample queries for composing test cases

func init() {
	var err error
	// Prepare two A queries on "github.com" for test cases
	githubComTCPQuery, err = hex.DecodeString("00274cc7012000010000000000010667697468756203636f6d00000100010000291000000000000000")
	if err != nil {
		panic(err)
	}
	githubComUDPQuery, err = hex.DecodeString("e575012000010000000000010667697468756203636f6d00000100010000291000000000000000")
	if err != nil {
		panic(err)
	}
}
