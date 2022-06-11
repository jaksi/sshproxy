package main

import (
	"flag"
	"io"
	"log"

	"github.com/jaksi/sshutils"
	"golang.org/x/crypto/ssh"
)

func proxyChannels(client, server *sshutils.Channel) {
	go io.Copy(client, server)
	go io.Copy(server, client)
	go io.Copy(client.Stderr(), server.Stderr())
	go io.Copy(server.Stderr(), client.Stderr())
	for client.Requests != nil || server.Requests != nil {
		select {
		case request, ok := <-client.Requests:
			if !ok {
				log.Print("client channel requests closed")
				server.Close()
				client.Requests = nil
				continue
			}
			log.Printf("client channel request: %s", request.Type)
			accepted, err := server.RawRequest(request.Type, request.WantReply, request.Payload)
			if err != nil {
				panic(err)
			}
			if err := request.Reply(accepted, nil); err != nil {
				panic(err)
			}
		case request, ok := <-server.Requests:
			if !ok {
				log.Print("server channel requests closed")
				client.Close()
				server.Requests = nil
				continue
			}
			log.Printf("server channel request: %s", request.Type)
			accepted, err := client.RawRequest(request.Type, request.WantReply, request.Payload)
			if err != nil {
				panic(err)
			}
			if err := request.Reply(accepted, nil); err != nil {
				panic(err)
			}
		}
	}
}

func proxyConnections(client, server *sshutils.Conn) {
	for client.Requests != nil || client.NewChannels != nil ||
		server.Requests != nil || server.NewChannels != nil {
		select {
		case request, ok := <-client.Requests:
			if !ok {
				log.Print("client requests closed")
				server.Close()
				client.Requests = nil
				continue
			}
			log.Printf("client request: %s", request.Type)
			accepted, response, err := server.RawRequest(request.Type, request.WantReply, request.Payload)
			if err != nil {
				panic(err)
			}
			if err := request.Reply(accepted, response); err != nil {
				panic(err)
			}
		case request, ok := <-server.Requests:
			if !ok {
				log.Print("server requests closed")
				client.Close()
				server.Requests = nil
				continue
			}
			log.Printf("server request: %s", request.Type)
			accepted, response, err := client.RawRequest(request.Type, request.WantReply, request.Payload)
			if err != nil {
				panic(err)
			}
			if err := request.Reply(accepted, response); err != nil {
				panic(err)
			}
		case newChannel, ok := <-client.NewChannels:
			if !ok {
				log.Print("client new channels closed")
				client.NewChannels = nil
				continue
			}
			log.Printf("client new channel: %s", newChannel.ChannelType())
			serverChannel, err := server.RawChannel(newChannel.ChannelType(), newChannel.ExtraData())
			if err != nil {
				if openChannelErr, ok := err.(*ssh.OpenChannelError); ok {
					if err := newChannel.Reject(openChannelErr.Reason, openChannelErr.Message); err != nil {
						panic(err)
					}
					continue
				}
				panic(err)
			}
			clientChannel, err := newChannel.AcceptChannel()
			if err != nil {
				panic(err)
			}
			go proxyChannels(clientChannel, serverChannel)
		case newChannel, ok := <-server.NewChannels:
			if !ok {
				log.Print("server new channels closed")
				server.NewChannels = nil
				continue
			}
			log.Printf("server new channel: %s", newChannel.ChannelType())
			clientChannel, err := client.RawChannel(newChannel.ChannelType(), newChannel.ExtraData())
			if err != nil {
				if openChannelErr, ok := err.(*ssh.OpenChannelError); ok {
					if err := newChannel.Reject(openChannelErr.Reason, openChannelErr.Message); err != nil {
						panic(err)
					}
					continue
				}
				panic(err)
			}
			serverChannel, err := newChannel.AcceptChannel()
			if err != nil {
				panic(err)
			}
			go proxyChannels(clientChannel, serverChannel)
		}
	}
}

func main() {
	listenAddress := flag.String("listen", "", "address to listen on")
	hostKeyFile := flag.String("hostkey", "", "host key file")
	serverAddress := flag.String("connect", "", "address to connect to")
	user := flag.String("user", "", "user to connect as")
	password := flag.String("password", "", "password to connect with")
	keyFile := flag.String("key", "", "key to connect with")
	flag.Parse()

	var serverConn *sshutils.Conn
	if *listenAddress != "" {
		if *hostKeyFile == "" {
			panic("hostkey is required when listen is specified")
		}
		hostKey, err := sshutils.LoadHostKey(*hostKeyFile)
		if err != nil {
			panic(err)
		}
		serverConfig := &ssh.ServerConfig{
			NoClientAuth: true,
		}
		serverConfig.AddHostKey(hostKey)
		listener, err := sshutils.Listen(*listenAddress, serverConfig)
		if err != nil {
			panic(err)
		}
		serverConn, err = listener.Accept()
		if err != nil {
			panic(err)
		}
		listener.Close()
	}

	var clientConn *sshutils.Conn
	if *serverAddress != "" {
		if *user == "" {
			panic("user is required when server is specified")
		}
		clientConfig := &ssh.ClientConfig{
			User:            *user,
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		}
		if *password != "" {
			clientConfig.Auth = append(clientConfig.Auth, ssh.Password(*password))
		}
		if *keyFile != "" {
			key, err := sshutils.LoadHostKey(*keyFile)
			if err != nil {
				panic(err)
			}
			clientConfig.Auth = append(clientConfig.Auth, ssh.PublicKeys(key))
		}
		var err error
		clientConn, err = sshutils.Dial(*serverAddress, clientConfig)
		if err != nil {
			panic(err)
		}
	}

	if serverConn != nil && clientConn != nil {
		log.Printf("Running in proxy mode")
		proxyConnections(serverConn, clientConn)
	} else if serverConn != nil {
		log.Printf("Running in client mode")
	} else if clientConn != nil {
		log.Printf("Running in server mode")
	} else {
		panic("either listen or connect is required")
	}
}
