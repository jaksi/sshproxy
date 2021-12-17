package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/jaksi/sshutils"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

type command struct {
	aliases     []string
	description string
	usage       string
	action      func(args []string) error
}

var (
	exit  = errors.New("exit")
	usage = errors.New("usage")
)

type side int

const (
	client side = iota
	server
)

func (s side) String() string {
	switch s {
	case client:
		return "client"
	case server:
		return "server"
	default:
		return "unknown"
	}
}

type conn struct {
	*sshutils.Conn
	id       int
	side     side
	channels []*sshutils.Channel
}

var (
	terminal    *term.Terminal
	connections []*conn
	mutex       = &sync.Mutex{}
	maxId       int
)

func handleGlobalRequest(connection *conn, request *ssh.Request) error {
	mutex.Lock()
	defer mutex.Unlock()
	payloadString := base64.StdEncoding.EncodeToString(request.Payload)
	accepted := false
	var response []byte
	payload, err := sshutils.UnmarshalGlobalRequestPayload(request)
	if err == nil {
		payloadString = payload.String()
		accepted = true
	}
	fmt.Fprintf(terminal, "< %v global_request type=%q want_reply=%v payload=%q\n", connection.id, request.Type, request.WantReply, payloadString)
	fmt.Fprintf(terminal, " > accepted=%v response=%q\n", accepted, base64.RawStdEncoding.EncodeToString(response))
	return request.Reply(accepted, response)
}

func handleChannelRequest(connection *conn, channel *sshutils.Channel, request *ssh.Request) error {
	mutex.Lock()
	defer mutex.Unlock()
	payloadString := base64.StdEncoding.EncodeToString(request.Payload)
	accepted := false
	var response []byte
	payload, err := sshutils.UnmarshalChannelRequestPayload(request)
	if err == nil {
		payloadString = payload.String()
		accepted = true
	}
	fmt.Fprintf(terminal, "< %v:%v channel_request type=%q want_reply=%v payload=%q\n", connection.id, channel.ChannelID(), request.Type, request.WantReply, payloadString)
	fmt.Fprintf(terminal, " > accepted=%v response=%q\n", accepted, base64.RawStdEncoding.EncodeToString(response))
	return request.Reply(accepted, response)
}

func handleChannel(connection *conn, channel *sshutils.Channel) {
	defer func() {
		mutex.Lock()
		for i, c := range connection.channels {
			if c == channel {
				connection.channels = append(connection.channels[:i], connection.channels[i+1:]...)
				break
			}
		}
		mutex.Unlock()
		_ = channel.CloseWrite()
		channel.Close()
	}()
	pty := false
	go func() {
		for request := range channel.Requests {
			if err := handleChannelRequest(connection, channel, request); err != nil {
				fmt.Fprintf(terminal, "error handling request: %v\n", err)
			}
			if channel.ChannelType() == "session" && request.Type == "pty-req" {
				pty = true
			}
		}
	}()
	buffer := make([]byte, 1024)
	for {
		n, err := channel.Read(buffer)
		if err != nil {
			fmt.Fprintf(terminal, "error reading channel: %v\n", err)
			return
		}
		if n > 0 {
			fmt.Fprintf(terminal, "< %v:%v %q\n", connection.id, channel, string(buffer[:n]))
		}
		if pty {
			if _, err := channel.Write([]byte(strings.ReplaceAll(string(buffer[:n]), "\r", "\r\n"))); err != nil {
				fmt.Fprintf(terminal, "error writing channel: %v\n", err)
				return
			}
		}
	}
}

func handleNewChannel(connection *conn, newChannel *sshutils.NewChannel) error {
	mutex.Lock()
	defer mutex.Unlock()
	payloadString := base64.StdEncoding.EncodeToString(newChannel.ExtraData())
	accepted := false
	payload, err := newChannel.Payload()
	if err == nil {
		payloadString = payload.String()
		accepted = true
	}
	fmt.Fprintf(terminal, "< %v new_channel type=%q payload=%q\n", connection.id, newChannel.ChannelType(), payloadString)
	if !accepted {
		fmt.Fprintf(terminal, " > accepted=%v\n", accepted)
		return newChannel.Reject(ssh.Prohibited, "channel type not accepted")
	}
	channel, err := newChannel.AcceptChannel()
	if err != nil {
		return err
	}
	fmt.Fprintf(terminal, " > accepted=%v channel_id=%v\n", accepted, channel.ChannelID())
	connection.channels = append(connection.channels, channel)
	go handleChannel(connection, channel)
	return nil
}

func handleConn(connection *conn) {
	defer func() {
		mutex.Lock()
		for i, c := range connections {
			if c.Conn == connection.Conn {
				connections = append(connections[:i], connections[i+1:]...)
				break
			}
		}
		mutex.Unlock()
		connection.Close()
	}()
	for {
		select {
		case request, ok := <-connection.Requests:
			if !ok {
				return
			}
			if err := handleGlobalRequest(connection, request); err != nil {
				fmt.Fprintf(terminal, "error handling request: %v\n", err)
			}
		case newChannel, ok := <-connection.NewChannels:
			if !ok {
				return
			}
			if err := handleNewChannel(connection, newChannel); err != nil {
				fmt.Fprintf(terminal, "error handling new channel: %v\n", err)
			}
		}
	}
}

var commands = []command{
	{
		aliases:     []string{"exit", "quit", "q"},
		description: "exit the program",
		usage:       "",
		action: func(args []string) error {
			if len(args) != 0 {
				return usage
			}
			return exit
		},
	},
	{
		aliases:     []string{"connect-password", "cp"},
		description: "connect to a server using a password",
		usage:       "<address> <user> <password>",
		action: func(args []string) error {
			if len(args) != 3 {
				return usage
			}
			address := args[0]
			user := args[1]
			password := args[2]
			c, err := sshutils.Dial(address, &ssh.ClientConfig{
				User:            user,
				Auth:            []ssh.AuthMethod{ssh.Password(password)},
				HostKeyCallback: ssh.InsecureIgnoreHostKey(),
			})
			if err != nil {
				return err
			}
			mutex.Lock()
			connection := &conn{c, maxId, client, []*sshutils.Channel{}}
			maxId++
			connections = append(connections, connection)
			mutex.Unlock()
			fmt.Fprintf(terminal, "connection_id=%v\n", connection.id)
			go handleConn(connection)
			return nil
		},
	},
	{
		aliases:     []string{"connect-key", "ck"},
		description: "connect to a server using a private key",
		usage:       "<address> <user> <private key>",
		action: func(args []string) error {
			if len(args) != 3 {
				return usage
			}
			address := args[0]
			user := args[1]
			keyFile := args[2]
			key, err := sshutils.LoadHostKey(keyFile)
			if err != nil {
				return err
			}
			c, err := sshutils.Dial(address, &ssh.ClientConfig{
				User:            user,
				Auth:            []ssh.AuthMethod{ssh.PublicKeys(key)},
				HostKeyCallback: ssh.InsecureIgnoreHostKey(),
			})
			if err != nil {
				return err
			}
			mutex.Lock()
			connection := &conn{c, maxId, client, []*sshutils.Channel{}}
			maxId++
			connections = append(connections, connection)
			mutex.Unlock()
			fmt.Fprintf(terminal, "connection_id=%v\n", connection.id)
			go handleConn(connection)
			return nil
		},
	},
	{
		aliases:     []string{"accept", "a"},
		description: "listen on the specified address and accept a single connection",
		usage:       "<address> [<host_key_file>]",
		action: func(args []string) error {
			if len(args) < 1 || len(args) > 2 {
				return usage
			}
			address := args[0]
			var hostKey *sshutils.HostKey
			var err error
			if len(args) == 2 {
				keyFile := args[1]
				hostKey, err = sshutils.LoadHostKey(keyFile)
			} else {
				hostKey, err = sshutils.GenerateHostKey(sshutils.ECDSA)
			}
			if err != nil {
				return err
			}

			config := &ssh.ServerConfig{
				NoClientAuth: true,
			}
			config.AddHostKey(hostKey)

			listener, err := sshutils.Listen(address, config)
			if err != nil {
				return err
			}
			defer listener.Close()

			fmt.Fprintf(terminal, "listening on %v, host key: %v\n", listener.Addr(), hostKey)
			c, err := listener.Accept()
			if err != nil {
				return err
			}
			mutex.Lock()
			connection := &conn{c, maxId, client, []*sshutils.Channel{}}
			maxId++
			connections = append(connections, connection)
			mutex.Unlock()
			fmt.Fprintf(terminal, "connection_id=%v remote_address=%v\n", connection.id, c.RemoteAddr())
			go handleConn(connection)
			return nil
		},
	},
	{
		aliases:     []string{"ls", "l"},
		description: "list active connections",
		usage:       "",
		action: func(args []string) error {
			if len(args) != 0 {
				return usage
			}
			mutex.Lock()
			for _, connection := range connections {
				fmt.Fprintf(terminal, "%v: %v, %v\n", connection.id, connection.side, connection.RemoteAddr())
				for _, channel := range connection.channels {
					fmt.Fprintf(terminal, "  %v: %v\n", channel, channel.ChannelType())
				}
			}
			mutex.Unlock()
			return nil
		},
	},
	{
		aliases:     []string{"write", "w"},
		description: "write data to a channel",
		usage:       "<connection> <channel> <data ...>",
		action: func(args []string) error {
			if len(args) < 3 {
				return usage
			}
			connectionId, err := strconv.Atoi(args[0])
			if err != nil {
				return err
			}
			channelId, err := strconv.Atoi(args[1])
			if err != nil {
				return err
			}
			data := strings.Join(args[2:], " ")
			mutex.Lock()
			defer mutex.Unlock()
			fmt.Fprintf(terminal, "> %v:%v %q\n", connectionId, channelId, data)
			connection := connections[connectionId]
			channel := connection.channels[channelId]
			_, err = channel.Write([]byte(data))
			return err
		},
	},
	{
		aliases:     []string{"write-b64", "wb"},
		description: "write base64-encoded data to a channel",
		usage:       "<connection> <channel> <b64_data>",
		action: func(args []string) error {
			if len(args) != 3 {
				return usage
			}
			connectionId, err := strconv.Atoi(args[0])
			if err != nil {
				return err
			}
			channelId, err := strconv.Atoi(args[1])
			if err != nil {
				return err
			}
			data, err := base64.StdEncoding.DecodeString(args[2])
			if err != nil {
				return err
			}
			mutex.Lock()
			defer mutex.Unlock()
			fmt.Fprintf(terminal, "> %v:%v %q\n", connectionId, channelId, data)
			connection := connections[connectionId]
			channel := connection.channels[channelId]
			_, err = channel.Write(data)
			return err
		},
	},
	{
		aliases:     []string{"global-request", "gr"},
		description: "send a global request",
		usage:       "<connection> <type> <want reply> [<b64 payload>]",
		action: func(args []string) error {
			if len(args) < 3 || len(args) > 4 {
				return usage
			}
			connectionId, err := strconv.Atoi(args[0])
			if err != nil {
				return err
			}
			requestType := args[1]
			wantReply, err := strconv.ParseBool(args[2])
			if err != nil {
				return err
			}
			var payload []byte
			if len(args) == 4 {
				payload, err = base64.StdEncoding.DecodeString(args[3])
				if err != nil {
					return err
				}
			}
			mutex.Lock()
			defer mutex.Unlock()
			connection := connections[connectionId]
			fmt.Fprintf(terminal, "> %v global_request type=%q want_reply=%v payload=%q\n", connectionId, requestType, wantReply, payload)
			accepted, response, err := connection.SendRequest(requestType, wantReply, payload)
			if err != nil {
				return err
			}
			fmt.Fprintf(terminal, " < accepted=%v response=%q\n", accepted, response)
			return nil
		},
	},
	{
		aliases:     []string{"channel-request", "cr"},
		description: "send a channel request",
		usage:       "<connection> <channel> <type> <want reply> [<b64 payload>]",
		action: func(args []string) error {
			if len(args) < 4 || len(args) > 5 {
				return usage
			}
			connectionId, err := strconv.Atoi(args[0])
			if err != nil {
				return err
			}
			channelId, err := strconv.Atoi(args[1])
			if err != nil {
				return err
			}
			requestType := args[2]
			wantReply, err := strconv.ParseBool(args[3])
			if err != nil {
				return err
			}
			var payload []byte
			if len(args) == 5 {
				payload, err = base64.StdEncoding.DecodeString(args[4])
				if err != nil {
					return err
				}
			}
			mutex.Lock()
			defer mutex.Unlock()
			connection := connections[connectionId]
			channel := connection.channels[channelId]
			fmt.Fprintf(terminal, "> %v:%v channel_request type=%q want_reply=%v payload=%q\n", connectionId, channelId, requestType, wantReply, payload)
			accepted, err := channel.SendRequest(requestType, wantReply, payload)
			if err != nil {
				return err
			}
			fmt.Fprintf(terminal, " < accepted=%v\n", accepted)
			return nil
		},
	},
}

func init() {
	commands = append(commands, command{
		aliases:     []string{"help", "h"},
		description: "list available commands",
		usage:       "",
		action: func(args []string) error {
			if len(args) != 0 {
				return usage
			}
			for _, cmd := range commands {
				fmt.Fprintf(terminal, "%s\n%s\nusage: %s %s\n\n", strings.Join(cmd.aliases, "|"), cmd.description, cmd.aliases[0], cmd.usage)
			}
			return nil
		},
	})
}

func main() {
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		panic(err)
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState) //nolint:errcheck
	terminal = term.NewTerminal(os.Stdin, "sshdbg> ")
	terminal.AutoCompleteCallback = func(line string, pos int, key rune) (newLine string, newPos int, ok bool) {
		if key != '\t' {
			return line, pos, false
		}
		if pos != len(line) {
			return line, pos, false
		}
		for _, cmd := range commands {
			for _, alias := range cmd.aliases {
				if strings.HasPrefix(alias, line) {
					return alias, len(alias), true
				}
			}
		}
		return line, pos, false
	}
	for {
		line, err := terminal.ReadLine()
		if err != nil {
			break
		}
		args := strings.Fields(line)
		if len(args) == 0 {
			continue
		}
		var cmd *command
		for _, c := range commands {
			for _, a := range c.aliases {
				if a == args[0] {
					cmd = &c
					break
				}
			}
			if cmd != nil {
				break
			}
		}
		if cmd == nil {
			fmt.Fprintf(terminal, "unknown command: %s\n", args[0])
			continue
		}
		if err := cmd.action(args[1:]); err != nil {
			if err == exit {
				break
			}
			if err == usage {
				fmt.Fprintf(terminal, "usage: %s %s\n", args[0], cmd.usage)
				continue
			}
			fmt.Fprintf(terminal, "error: %s\n", err)
		}
	}
}
