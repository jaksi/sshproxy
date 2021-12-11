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
	id   int
	side side
}

var (
	terminal    *term.Terminal
	connections []conn
	mutex       = &sync.Mutex{}
	maxId       int
)

func handleConn(connection conn) {
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
			fmt.Fprintf(terminal, "%v: global request: %v\n", connection.id, request.Type)
			if err := request.Reply(false, nil); err != nil {
				fmt.Fprintf(terminal, "%v: error replying to global request: %v\n", connection, err)
			}
		case newChannel, ok := <-connection.NewChannels:
			if !ok {
				return
			}
			fmt.Fprintf(terminal, "%v: new channel: %v\n", connection.id, newChannel)
			if err := newChannel.Reject(ssh.Prohibited, "no channels allowed"); err != nil {
				fmt.Fprintf(terminal, "error rejecting channel: %v\n", err)
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
			connection := conn{c, maxId, client}
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
			connection := conn{c, maxId, client}
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
				fmt.Fprintf(terminal, "%s\n%s\nUsage: %s %s\n\n", strings.Join(cmd.aliases, "|"), cmd.description, cmd.aliases[0], cmd.usage)
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
				fmt.Fprintf(terminal, "Usage: %s %s\n", args[0], cmd.usage)
				continue
			}
			fmt.Fprintf(terminal, "error: %s\n", err)
		}
	}
}
