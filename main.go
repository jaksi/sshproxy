package main

import (
	"errors"
	"fmt"
	"os"
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
	fmt.Fprintf(terminal, "%v: global request: %v\n", connection.id, request.Type)
	payload, err := sshutils.UnmarshalGlobalRequestPayload(request)
	if err != nil {
		if err := request.Reply(false, nil); err != nil {
			return err
		}
		return err
	}
	fmt.Fprintf(terminal, "payload: %v\n", payload)
	if err := request.Reply(true, nil); err != nil {
		return err
	}
	return nil
}

func handleChannelRequest(connection *conn, channel *sshutils.Channel, request *ssh.Request) error {
	mutex.Lock()
	defer mutex.Unlock()
	fmt.Fprintf(terminal, "%v: %v: channel request: %v\n", connection.id, channel, request.Type)
	payload, err := sshutils.UnmarshalChannelRequestPayload(request)
	if err != nil {
		if err := request.Reply(false, nil); err != nil {
			return err
		}
		return err
	}
	fmt.Fprintf(terminal, "payload: %v\n", payload)
	if err := request.Reply(true, nil); err != nil {
		return err
	}
	return nil
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
			fmt.Fprintf(terminal, "%v: %v: channel data: %q\n", connection.id, channel, string(buffer[:n]))
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
	fmt.Fprintf(terminal, "%v: new channel: %v\n", connection.id, newChannel)
	payload, err := newChannel.Payload()
	if err != nil {
		if err := newChannel.Reject(ssh.UnknownChannelType, "unknown channel type"); err != nil {
			return err
		}
		return err
	}
	fmt.Fprintf(terminal, "payload: %v\n", payload)
	channel, err := newChannel.AcceptChannel()
	if err != nil {
		return err
	}
	connection.channels = append(connection.channels, channel)
	fmt.Fprintf(terminal, "accepted: %v\n", channel)
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
		aliases:     []string{"exit", "quit"},
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
		aliases:     []string{"connect"},
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
			fmt.Fprintf(terminal, "connected: %v\n", connection.id)
			go handleConn(connection)
			return nil
		},
	},
	{
		aliases:     []string{"connect-key"},
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
			fmt.Fprintf(terminal, "connected: %v\n", connection.id)
			go handleConn(connection)
			return nil
		},
	},
	{
		aliases:     []string{"ls"},
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
