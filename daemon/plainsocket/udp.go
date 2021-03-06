package plainsocket

import (
	"bufio"
	"bytes"
	"fmt"
	"github.com/HouzuoGuo/laitos/misc"
	"github.com/HouzuoGuo/laitos/testingstub"
	"github.com/HouzuoGuo/laitos/toolbox"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

const (
	MaxPacketSize = 9038 // Maximum acceptable UDP packet size
)

var UDPDurationStats = misc.NewStats() // UDPDurationStats stores statistics of duration of all UDP conversations.

/*
You may call this function only after having called Initialise()!
Start UDP daemon and block until daemon is told to stop.
*/
func (daemon *Daemon) StartAndBlockUDP() error {
	listenAddr := fmt.Sprintf("%s:%d", daemon.Address, daemon.UDPPort)
	udpAddr, err := net.ResolveUDPAddr("udp", listenAddr)
	if err != nil {
		return err
	}
	udpServer, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return err
	}
	defer udpServer.Close()
	daemon.udpListener = udpServer
	daemon.logger.Printf("StartAndBlockUDP", listenAddr, nil, "going to listen for commands")
	// Process incoming requests
	packetBuf := make([]byte, MaxPacketSize)
	for {
		if misc.EmergencyLockDown {
			return misc.ErrEmergencyLockDown
		}
		packetLength, clientAddr, err := udpServer.ReadFromUDP(packetBuf)
		if err != nil {
			if strings.Contains(err.Error(), "closed") {
				return nil
			}
			return fmt.Errorf("plainsocket.StartAndBlockUDP: failed to accept new connection - %v", err)
		}
		// Check IP address against (connection) rate limit
		clientIP := clientAddr.IP.String()
		if !daemon.rateLimit.Add(clientIP, true) {
			continue
		}

		clientPacket := make([]byte, packetLength)
		copy(clientPacket, packetBuf[:packetLength])
		go daemon.HandleUDPConnection(clientIP, clientAddr, clientPacket)
	}
}

// Read a feature command from each input line, then invoke the requested feature and write the execution result back to client.
func (daemon *Daemon) HandleUDPConnection(clientIP string, clientAddr *net.UDPAddr, packet []byte) {
	listener := daemon.udpListener
	if listener == nil {
		daemon.logger.Warningf("HandleUDPConnection", clientIP, nil, "listener is closed before request can be processed")
		return
	}
	// Put processing duration (including IO time) into statistics
	beginTimeNano := time.Now().UnixNano()
	defer func() {
		UDPDurationStats.Trigger(float64(time.Now().UnixNano() - beginTimeNano))
	}()
	// Unlike TCP, there's no point in checking against rate limit for the connection itself.
	daemon.logger.Printf("HandleUDPConnection", clientIP, nil, "working on the connection")
	reader := bufio.NewReader(bytes.NewReader(packet))
	for {
		// Read one line of command
		line, _, err := reader.ReadLine()
		if err != nil {
			if err != io.EOF {
				daemon.logger.Warningf("HandleUDPConnection", clientIP, err, "failed to read received packet")
			}
			return
		}
		// Check against conversation rate limit
		if !daemon.rateLimit.Add(clientIP, true) {
			return
		}
		// Process line of command and respond
		result := daemon.Processor.Process(toolbox.Command{Content: string(line), TimeoutSec: CommandTimeoutSec})
		daemon.udpListener.SetWriteDeadline(time.Now().Add(IOTimeoutSec * time.Second))
		if _, err := daemon.udpListener.WriteToUDP([]byte(result.CombinedOutput), clientAddr); err != nil {
			daemon.logger.Warningf("HandleUDPConnection", clientIP, err, "failed to write response")
			return
		}
		if _, err := daemon.udpListener.WriteToUDP([]byte("\r\n"), clientAddr); err != nil {
			daemon.logger.Warningf("HandleUDPConnection", clientIP, err, "failed to write response")
			return
		}
	}
}

// Run unit tests on the UDP server. See TestPlainTextProt_StartAndBlockUDP for daemon setup.
func TestUDPServer(server *Daemon, t testingstub.T) {
	// Prevent daemon from listening to TCP connections in this UDP test case
	tcpListenPort := server.TCPPort
	server.TCPPort = 0
	defer func() {
		server.TCPPort = tcpListenPort
	}()
	// Server should start within two seconds
	var stoppedNormally bool
	go func() {
		if err := server.StartAndBlock(); err != nil {
			t.Fatal(err)
		}
		stoppedNormally = true
	}()
	time.Sleep(2 * time.Second)

	// Try to exceed rate limit
	success := 0
	for i := 0; i < 30; i++ {
		clientConn, err := net.Dial("udp", "127.0.0.1:"+strconv.Itoa(server.UDPPort))
		if err != nil {
			t.Fatal(err)
		}
		_, err = clientConn.Write([]byte("verysecret .s echo hi\r\n"))
		if err != nil {
			t.Fatal(err)
		}
		clientConn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		goodPINResp, _, err := bufio.NewReader(clientConn).ReadLine()
		if err != nil {
			continue
		}
		if string(goodPINResp) != "hi" {
			t.Fatal(string(goodPINResp))
		}
		clientConn.Close()
		success++
	}
	if success < 5 || success > 15 {
		t.Fatal("succeeded", success)
	}
	// Wait till rate limit expires
	time.Sleep(RateLimitIntervalSec * time.Second)

	// Make two normal conversations
	clientConn, err := net.Dial("udp", "127.0.0.1:"+strconv.Itoa(server.UDPPort))
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()
	reader := bufio.NewReader(clientConn)
	// Command with bad PIN
	_, err = clientConn.Write([]byte("pin mismatch\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	badPINResp, _, err := reader.ReadLine()
	if err != nil {
		t.Fatal(err)
	}
	if string(badPINResp) != "Failed to match PIN/shortcut" {
		t.Fatal(string(badPINResp))
	}
	// With good PIN
	_, err = clientConn.Write([]byte("verysecret .s echo hi\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	goodPINResp, _, err := reader.ReadLine()
	if err != nil {
		t.Fatal(err)
	}
	if string(goodPINResp) != "hi" {
		t.Fatal(string(goodPINResp))
	}

	// Daemon should stop within a second
	server.Stop()
	time.Sleep(1 * time.Second)
	if !stoppedNormally {
		t.Fatal("did not stop")
	}
	// Repeatedly stopping the daemon should have no negative consequence
	server.Stop()
	server.Stop()
}
