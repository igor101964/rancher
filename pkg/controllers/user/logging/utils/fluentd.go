package utils

import (
	"crypto/sha512"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/rancher/norman/types/convert"
	"github.com/rancher/types/apis/management.cattle.io/v3"
	"github.com/rancher/types/config/dialer"
	"github.com/vmihailenco/msgpack"
)

var (
	letterHex                = "0123456789abcdef"
	fluentdForwarderTestData = []byte(fmt.Sprintf(`["rancher",[[ %d, {"message": "`+testMessage+`"}]]]`, time.Now().Unix()))
)

type fluentForwarderTestWrap struct {
	*v3.FluentForwarderConfig
}

type heloOption struct {
	Nonce     string `json:"nonce"`
	Auth      string `json:"auth"`
	Keepalive bool   `json:"keepalive"`
}

func (w *fluentForwarderTestWrap) TestReachable(dial dialer.Dialer, includeSendTestLog bool) error {
	var err error

	for _, s := range w.FluentServers {
		var tlsConfig *tls.Config
		if w.EnableTLS {
			serverName := s.Hostname
			if serverName == "" {
				host, _, err := net.SplitHostPort(s.Endpoint)
				if err != nil {
					return errors.Wrapf(err, "couldn't parse url %s", s.Endpoint)
				}
				serverName = host
			}
			tlsConfig, err = buildTLSConfig(w.Certificate, "", "", "", "", serverName, true)
			if err != nil {
				return err
			}
		}

		conn, err := newTCPConn(dial, s.Endpoint, tlsConfig, true)
		if err != nil {
			return err
		}

		if !includeSendTestLog {
			conn.Close()
			continue
		}

		if err := w.sendData2Server(conn, s.SharedKey, s.Username, s.Password, s.Endpoint); err != nil {
			conn.Close()
			return err
		}

		conn.Close()
	}
	return nil
}

func (w *fluentForwarderTestWrap) sendData2Server(conn net.Conn, shareKey, username, password, endpoint string) error {
	if shareKey == "" && username == "" && password == "" {
		if _, err := conn.Write(fluentdForwarderTestData); err != nil {
			return errors.Wrapf(err, "couldn't write data to fluentd forwarder %s", endpoint)
		}
	}

	buf := make([]byte, 1024)
	if _, err := conn.Read(buf); err != nil && err != io.EOF {
		return errors.Wrapf(err, "couldn't read data from fluentd forwarder %s", endpoint)
	}

	var heloBody []interface{}
	if err := msgpack.Unmarshal(buf, &heloBody); err != nil {
		return errors.Wrap(err, "couldn't unmarshal helo message")
	}

	if len(heloBody) < 2 {
		return errors.New("received invalid helo message")
	}

	var heloOption heloOption
	if err := convert.ToObj(heloBody[1], &heloOption); err != nil {
		return errors.Wrap(err, "couldn't convert helo body")
	}

	nonce, err := base64.StdEncoding.DecodeString(heloOption.Nonce)
	if err != nil {
		return errors.Wrap(err, "couldn't decode nonce from helo body")
	}

	auth, err := base64.StdEncoding.DecodeString(heloOption.Auth)
	if err != nil {
		return errors.Wrap(err, "couldn't decode auth from helo body")
	}

	ping, err := w.generateFluentForwarderPing(shareKey, string(nonce), username, password, string(auth))
	if err != nil {
		return errors.Wrap(err, "couldn't generate ping request")
	}

	if _, err = conn.Write([]byte(ping)); err != nil {
		return errors.Wrap(err, "couldn't send ping request to fluentd forwarder")
	}

	if _, err = conn.Write(fluentdForwarderTestData); err != nil {
		return errors.Wrap(err, "couldn't write test data to fluentd forwarder")
	}

	pongBuf := make([]byte, 1024)
	if _, err = conn.Read(pongBuf); err != nil && err != io.EOF {
		return errors.Wrap(err, "couldn't read pong data from fluentd forwarder")
	}

	return w.decodeFluentForwarderPong(pongBuf)
}

func (w *fluentForwarderTestWrap) generateFluentForwarderPing(shareKey, nonce, username, password, auth string) (string, error) {
	// format from fluentd code: ['PING', self_hostname, shared_key_salt, sha512_hex(shared_key_salt + self_hostname + nonce + shared_key), username || '', sha512_hex(auth_salt + username + password) || '']
	hostname, err := os.Hostname()
	if err != nil {
		return "", errors.Wrap(err, "couldn't get hostname")
	}

	salt := randHex(16)
	fullSharedKey := fmt.Sprintf("%s%s%s%s", salt, hostname, nonce, shareKey)
	hash := sha512.New()
	hash.Write([]byte(fullSharedKey))
	sharedKeyHex := hex.EncodeToString(hash.Sum(nil))

	passwordHex := ""
	if auth != "" {
		fullPassword := fmt.Sprintf("%s%s%s", auth, username, password)
		passwordHash := sha512.New()
		passwordHash.Write([]byte(fullPassword))
		passwordHex = hex.EncodeToString(passwordHash.Sum(nil))
	}
	return fmt.Sprintf(`["PING", "%s", "%s", "%s", "%s", "%s"]`, hostname, salt, sharedKeyHex, username, passwordHex), nil
}

func (w *fluentForwarderTestWrap) decodeFluentForwarderPong(pong []byte) error {
	// format from fluentd code ['PONG', bool(authentication result), 'reason if authentication failed', self_hostname, sha512_hex(salt + self_hostname + nonce + sharedkey)]
	// sample:  ["PONG",false,"shared_key mismatch","",""]
	pongMsg := string(pong)
	pongMsg = strings.TrimPrefix(pongMsg, "[")
	pongMsg = strings.TrimSuffix(pongMsg, "]")
	pongMsgArray := strings.Split(pongMsg, ",")
	if len(pongMsgArray) != 5 {
		return errors.New("received invalid pong message, pong message: " + pongMsg)
	}

	if pongMsgArray[1] == "false" {
		return errors.New("auth failed, reason: " + pongMsgArray[2])
	}

	return nil
}
