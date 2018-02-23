// Copyright 2017 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package reverseproxy

import (
	"crypto/tls"
	"io"
	"net"
	"net/url"

	"github.com/nu7hatch/gouuid"
	"github.com/opencoff/planb/log"
)

// SNIReverseProxy is public struct for SNI reverseproxy
type SNIReverseProxy struct {
	ReverseProxyConfig
}

// Initialize is public interface used on main.go to initialize reverseproxy
func (rp *SNIReverseProxy) Initialize(rpConfig ReverseProxyConfig) error {
	rp.ReverseProxyConfig = rpConfig
	return nil
}

// Stop is public interface used on main to stop reverseproxy after os.Interrupt or os.Kill signal
func (rp *SNIReverseProxy) Stop() {
	// no special treatment for sni reverse proxy
}

// Listen is public interface used in router/listener.go
func (rp *SNIReverseProxy) Listen(listener net.Listener, tlsConfig *tls.Config) {
	for {
		connection, err := listener.Accept()
		connID, _ := uuid.NewV4()
		if err != nil {
			log.ErrorLogger.Print("ERROR in ACCEPT - ", listener.Addr(), " - ", connID.String(), " - ", err.Error())
			return
		}
		go rp.handleSNIConnection(connection, connID.String())
	}
}

func (rp *SNIReverseProxy) handleSNIConnection(downstream net.Conn, connID string) {
	defer downstream.Close()
	firstByte := make([]byte, 1)
	_, err := downstream.Read(firstByte)
	if err != nil {
		log.ErrorLogger.Print("ERROR - Couldn't read first byte - ", connID)
		return
	}
	if firstByte[0] != 0x16 {
		log.ErrorLogger.Print("ERROR - Not TLS - ", connID)
		return
	}

	versionBytes := make([]byte, 2)
	_, err = downstream.Read(versionBytes)
	if err != nil {
		log.ErrorLogger.Print("ERROR - Couldn't read version bytes - ", connID)
		return
	}
	if versionBytes[0] < 3 || (versionBytes[0] == 3 && versionBytes[1] < 1) {
		log.ErrorLogger.Print("ERROR -  SSL < 3.1 so it's still not TLS - ", connID)
		return
	}

	restLengthBytes := make([]byte, 2)
	_, err = downstream.Read(restLengthBytes)
	if err != nil {
		log.ErrorLogger.Print("ERROR - Couldn't read restLength bytes - ", connID)
		return
	}
	restLength := (int(restLengthBytes[0]) << 8) + int(restLengthBytes[1])

	rest := make([]byte, restLength)
	_, err = downstream.Read(rest)
	if err != nil {
		log.ErrorLogger.Print("ERROR - Couldn't read rest of bytes - ", connID)
		return
	}

	current := 0

	handshakeType := rest[0]
	current++
	if handshakeType != 0x1 {
		log.ErrorLogger.Print("ERROR - Not a ClientHello - ", connID)
		return
	}

	// Skip over another length
	current += 3
	// Skip over protocolversion
	current += 2
	// Skip over random number
	current += 4 + 28
	// Skip over session ID
	sessionIDLength := int(rest[current])
	current++
	current += sessionIDLength

	cipherSuiteLength := (int(rest[current]) << 8) + int(rest[current+1])
	current += 2
	current += cipherSuiteLength

	compressionMethodLength := int(rest[current])
	current++
	current += compressionMethodLength

	if current > restLength {
		log.ErrorLogger.Print("ERROR - no extensions - ", connID)
		return
	}

	// Skip over extensionsLength
	// extensionsLength := (int(rest[current]) << 8) + int(rest[current + 1])
	current += 2

	hostname := ""
	for current < restLength && hostname == "" {
		extensionType := (int(rest[current]) << 8) + int(rest[current+1])
		current += 2

		extensionDataLength := (int(rest[current]) << 8) + int(rest[current+1])
		current += 2

		if extensionType == 0 {

			// Skip over number of names as we're assuming there's just one
			current += 2

			nameType := rest[current]
			current++
			if nameType != 0 {
				log.ErrorLogger.Print("ERROR - Not a hostname - ", connID)
				return
			}
			nameLen := (int(rest[current]) << 8) + int(rest[current+1])
			current += 2
			hostname = string(rest[current : current+nameLen])
		}

		current += extensionDataLength
	}
	if hostname == "" {
		log.ErrorLogger.Print("ERROR - No hostname - ", connID)
		return
	}

	reqData, err := rp.Router.ChooseBackend(hostname)
	if err != nil {
		log.ErrorLogger.Print("ERROR - ChooseBackend - ", connID, " - ", err)
		return
	}
	url, err := url.Parse(reqData.Backend)
	if err != nil {
		log.ErrorLogger.Print("ERROR - url.Parse - ", connID, " - ", err)
		return
	}
	backendAddress := url.Host
	upstream, err := net.Dial("tcp", backendAddress)
	if err != nil {
		log.ErrorLogger.Print("ERROR - ConnectBackend - ", connID, " - ", err)
		return
	}
	defer upstream.Close()

	upstream.Write(firstByte)
	upstream.Write(versionBytes)
	upstream.Write(restLengthBytes)
	upstream.Write(rest)

	errc := make(chan error, 2)
	cp := func(dst io.Writer, src io.Reader) {
		_, err := io.Copy(dst, src)
		errc <- err
	}
	go cp(upstream, downstream)
	go cp(downstream, upstream)
	<-errc
}
