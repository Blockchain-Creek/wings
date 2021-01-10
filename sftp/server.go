package sftp

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"path"
	"strings"

	"emperror.dev/errors"
	"github.com/apex/log"
	"github.com/pkg/sftp"
	"github.com/pterodactyl/wings/api"
	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/server"
	"golang.org/x/crypto/ssh"
)

//goland:noinspection GoNameStartsWithPackageName
type SFTPServer struct {
	BasePath    string
	ReadOnly    bool
	BindPort    int
	BindAddress string
}

var noMatchingServerError = errors.Sentinel("sftp: no matching server with UUID")

func NewServer() *SFTPServer {
	cfg := config.Get().System
	return &SFTPServer{
		BasePath:    cfg.Data,
		ReadOnly:    cfg.Sftp.ReadOnly,
		BindAddress: cfg.Sftp.Address,
		BindPort:    cfg.Sftp.Port,
	}
}

// Starts the SFTP server and add a persistent listener to handle inbound SFTP connections.
func (c *SFTPServer) Run() error {
	serverConfig := &ssh.ServerConfig{
		NoClientAuth:     false,
		MaxAuthTries:     6,
		PasswordCallback: c.passwordCallback,
	}

	if _, err := os.Stat(path.Join(c.BasePath, ".sftp/id_rsa")); os.IsNotExist(err) {
		if err := c.generatePrivateKey(); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	privateBytes, err := ioutil.ReadFile(path.Join(c.BasePath, ".sftp/id_rsa"))
	if err != nil {
		return err
	}

	private, err := ssh.ParsePrivateKey(privateBytes)
	if err != nil {
		return err
	}

	// Add our private key to the server configuration.
	serverConfig.AddHostKey(private)

	listener, err := net.Listen("tcp", fmt.Sprintf("%s:%d", c.BindAddress, c.BindPort))
	if err != nil {
		return err
	}

	log.WithField("host", c.BindAddress).WithField("port", c.BindPort).Info("sftp subsystem listening for connections")

	for {
		conn, _ := listener.Accept()
		if conn != nil {
			go c.AcceptInboundConnection(conn, serverConfig)
		}
	}
}

// Handles an inbound connection to the instance and determines if we should serve the request
// or not.
func (c SFTPServer) AcceptInboundConnection(conn net.Conn, config *ssh.ServerConfig) {
	defer conn.Close()

	// Before beginning a handshake must be performed on the incoming net.Conn
	sconn, chans, reqs, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return
	}
	defer sconn.Close()

	go ssh.DiscardRequests(reqs)

	for newChannel := range chans {
		// If its not a session channel we just move on because its not something we
		// know how to handle at this point.
		if newChannel.ChannelType() != "session" {
			newChannel.Reject(ssh.UnknownChannelType, "unknown channel type")
			continue
		}

		channel, requests, err := newChannel.Accept()
		if err != nil {
			continue
		}

		// Channels have a type that is dependent on the protocol. For SFTP this is "subsystem"
		// with a payload that (should) be "sftp". Discard anything else we receive ("pty", "shell", etc)
		go func(in <-chan *ssh.Request) {
			for req := range in {
				ok := false

				switch req.Type {
				case "subsystem":
					if string(req.Payload[4:]) == "sftp" {
						ok = true
					}
				}

				req.Reply(ok, nil)
			}
		}(requests)

		if sconn.Permissions.Extensions["uuid"] == "" {
			continue
		}

		// Create a new handler for the currently logged in user's server.
		fs := c.newHandler(sconn)

		// Create the server instance for the channel using the filesystem we created above.
		handler := sftp.NewRequestServer(channel, fs)
		if err := handler.Serve(); err == io.EOF {
			handler.Close()
		}
	}
}

// Creates a new SFTP handler for a given server. The directory argument should
// be the base directory for a server. All actions done on the server will be
// relative to that directory, and the user will not be able to escape out of it.
func (c *SFTPServer) newHandler(sc *ssh.ServerConn) sftp.Handlers {
	s := server.GetServers().Find(func(s *server.Server) bool {
		return s.Id() == sc.Permissions.Extensions["uuid"]
	})

	p := Handler{
		fs:          s.Filesystem(),
		permissions: strings.Split(sc.Permissions.Extensions["permissions"], ","),
		ro:          config.Get().System.Sftp.ReadOnly,
		logger: log.WithFields(log.Fields{
			"subsystem": "sftp",
			"username":  sc.User(),
			"ip":        sc.RemoteAddr(),
		}),
	}

	return sftp.Handlers{
		FileGet:  &p,
		FilePut:  &p,
		FileCmd:  &p,
		FileList: &p,
	}
}

// Generates a private key that will be used by the SFTP server.
func (c *SFTPServer) generatePrivateKey() error {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(path.Join(c.BasePath, ".sftp"), 0755); err != nil {
		return err
	}

	o, err := os.OpenFile(path.Join(c.BasePath, ".sftp/id_rsa"), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer o.Close()

	pkey := &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}

	if err := pem.Encode(o, pkey); err != nil {
		return err
	}

	return nil
}

// A function capable of validating user credentials with the Panel API.
func (c *SFTPServer) passwordCallback(conn ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
	request := api.SftpAuthRequest{
		User:          conn.User(),
		Pass:          string(pass),
		IP:            conn.RemoteAddr().String(),
		SessionID:     conn.SessionID(),
		ClientVersion: conn.ClientVersion(),
	}

	logger := log.WithFields(log.Fields{"subsystem": "sftp", "username": conn.User(), "ip": conn.RemoteAddr().String()})
	logger.Debug("validating credentials for SFTP connection")

	resp, err := api.New().ValidateSftpCredentials(request)
	if err != nil {
		if api.IsInvalidCredentialsError(err) {
			logger.Warn("failed to validate user credentials (invalid username or password)")
		} else {
			logger.Error("encountered an error while trying to validate user credentials")
		}
		return nil, err
	}

	logger.WithField("server", resp.Server).Debug("credentials validated and matched to server instance")
	sshPerm := &ssh.Permissions{
		Extensions: map[string]string{
			"uuid":        resp.Server,
			"user":        conn.User(),
			"permissions": strings.Join(resp.Permissions, ","),
		},
	}

	return sshPerm, nil
}
