package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type IPMessenger struct {
	username     string
	listenPort   int
	contacts     map[string]Contact
	running      bool
	contactsFile string
	server       net.Listener
	wg           sync.WaitGroup
	mu           sync.Mutex
}

type Contact struct {
	IP   string `json:"ip"`
	Port int    `json:"port"`
}

func NewIPMessenger(username string, port int) (*IPMessenger, error) {
	messenger := &IPMessenger{
		username:     username,
		listenPort:   port,
		contacts:     make(map[string]Contact),
		running:      true,
		contactsFile: "contacts.json",
	}

	err := messenger.loadContacts()
	if err != nil {
		return nil, fmt.Errorf("failed to load contacts: %v", err)
	}

	err = messenger.startServer()
	if err != nil {
		return nil, fmt.Errorf("failed to start server: %v", err)
	}

	return messenger, nil
}

func (m *IPMessenger) loadContacts() error {
	file, err := os.Open(m.contactsFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // No contacts file yet
		}
		return err
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	err = decoder.Decode(&m.contacts)
	if err != nil {
		return err
	}

	fmt.Printf("[*] Loaded %d contacts\n", len(m.contacts))
	return nil
}

func (m *IPMessenger) saveContacts() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	file, err := os.Create(m.contactsFile)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(m.contacts)
}

func (m *IPMessenger) startServer() error {
	var err error
	m.server, err = net.Listen("tcp", fmt.Sprintf(":%d", m.listenPort))
	if err != nil {
		return err
	}

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		for m.running {
			conn, err := m.server.Accept()
			if err != nil {
				if m.running {
					fmt.Printf("[!] Server accept error: %v\n", err)
				}
				continue
			}

			m.wg.Add(1)
			go m.handleMessage(conn)
		}
	}()

	fmt.Printf("[*] Messenger started on port %d\n", m.listenPort)
	return nil
}

func (m *IPMessenger) handleMessage(conn net.Conn) {
	defer m.wg.Done()
	defer conn.Close()

	data, err := io.ReadAll(conn)
	if err != nil {
		fmt.Printf("[!] Connection read error: %v\n", err)
		return
	}

	fullData := strings.TrimSpace(string(data))
	if fullData == "" {
		return
	}

	// Try to parse as XML (XMPP-style message)
	if strings.HasPrefix(fullData, "<message") {
		from, body, err := parseXMPPMessage(fullData)
		if err != nil {
			fmt.Printf("[!] Error parsing XMPP message: %v\n", err)
		} else {
			timestamp := time.Now().Format("2006-01-02 15:04:05")
			fmt.Printf("\n[%s] %s: %s\nCommand > ", timestamp, from, body)
			os.Stdout.Sync() // flush output immediately
		}
	} else {
		// Plain message
		timestamp := time.Now().Format("2006-01-02 15:04:05")
		fmt.Printf("\n[%s] Unknown sender: %s\nCommand > ", timestamp, fullData)
		os.Stdout.Sync()
	}
}

func parseXMPPMessage(xml string) (from, body string, err error) {
	// Simple regex parsing - for a real application you'd want a proper XML parser
	fromRegex := regexp.MustCompile(`from="([^"]+)"`)
	bodyRegex := regexp.MustCompile(`<body>([^<]+)</body>`)

	fromMatch := fromRegex.FindStringSubmatch(xml)
	if len(fromMatch) < 2 {
		return "", "", errors.New("could not find 'from' attribute")
	}

	bodyMatch := bodyRegex.FindStringSubmatch(xml)
	if len(bodyMatch) < 2 {
		return fromMatch[1], "", nil
	}

	return fromMatch[1], bodyMatch[1], nil
}

func (m *IPMessenger) SendMessage(recipient, message string) error {
	m.mu.Lock()
	contact, exists := m.contacts[recipient]
	m.mu.Unlock()

	if !exists {
		return fmt.Errorf("contact '%s' not found", recipient)
	}

	xmppMsg := fmt.Sprintf(`<message to="%s" from="%s" type="chat" xmlns="jabber:client"><body>%s</body></message>`,
		recipient, m.username, message)

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", contact.IP, contact.Port), 5*time.Second)
	if err != nil {
		return fmt.Errorf("failed to connect: %v", err)
	}
	defer conn.Close()

	_, err = conn.Write([]byte(xmppMsg))
	if err != nil {
		return fmt.Errorf("failed to send message: %v", err)
	}

	fmt.Printf("[*] Message sent to %s\n", recipient)
	return nil
}

func (m *IPMessenger) AddContact(name, ip string, port int) error {
	if !isValidIP(ip) {
		return errors.New("invalid IP address")
	}

	if port < 1024 || port > 65535 {
		return errors.New("port must be between 1024 and 65535")
	}

	m.mu.Lock()
	m.contacts[name] = Contact{IP: ip, Port: port}
	m.mu.Unlock()

	err := m.saveContacts()
	if err != nil {
		return fmt.Errorf("failed to save contacts: %v", err)
	}

	fmt.Printf("[+] Added contact: %s (%s:%d)\n", name, ip, port)
	return nil
}

func isValidIP(ip string) bool {
	return net.ParseIP(ip) != nil
}

func (m *IPMessenger) RemoveContact(name string) error {
	m.mu.Lock()
	_, exists := m.contacts[name]
	if exists {
		delete(m.contacts, name)
	}
	m.mu.Unlock()

	if !exists {
		return fmt.Errorf("contact '%s' not found", name)
	}

	err := m.saveContacts()
	if err != nil {
		return fmt.Errorf("failed to save contacts: %v", err)
	}

	fmt.Printf("[-] Removed contact: %s\n", name)
	return nil
}

func (m *IPMessenger) EditContact(name, newIP string, newPort int) error {
	m.mu.Lock()
	_, exists := m.contacts[name]
	m.mu.Unlock()

	if !exists {
		return fmt.Errorf("contact '%s' not found", name)
	}

	return m.AddContact(name, newIP, newPort)
}

func (m *IPMessenger) ListContacts() {
	m.mu.Lock()
	defer m.mu.Unlock()

	fmt.Println("\n--- Contacts ---")
	for name, contact := range m.contacts {
		fmt.Printf("%s: %s:%d\n", name, contact.IP, contact.Port)
	}
	fmt.Println("---------------")
}

func (m *IPMessenger) Shutdown() {
	m.running = false
	if m.server != nil {
		m.server.Close()
	}
	m.wg.Wait()
	fmt.Println("[*] Messenger shutdown")
}

func main() {
	fmt.Println("XMPP-style IP Messenger")

	reader := bufio.NewReader(os.Stdin)

	// Get username
	var username string
	for {
		fmt.Print("Enter your username: ")
		input, err := reader.ReadString('\n')
		if err != nil {
			fmt.Println("[!] Error reading input:", err)
			continue
		}
		username = strings.TrimSpace(input)
		if username != "" {
			break
		}
		fmt.Println("[!] Username cannot be empty")
	}

	// Get port
	var port int
	for {
		fmt.Print("Enter port to use (default 12345): ")
		input, err := reader.ReadString('\n')
		if err != nil {
			fmt.Println("[!] Error reading input:", err)
			continue
		}
		input = strings.TrimSpace(input)
		if input == "" {
			port = 12345
			break
		}

		p, err := strconv.Atoi(input)
		if err != nil {
			fmt.Println("[!] Invalid port number")
			continue
		}

		if p >= 1024 && p <= 65535 {
			port = p
			break
		}
		fmt.Println("[!] Port must be between 1024 and 65535")
	}

	messenger, err := NewIPMessenger(username, port)
	if err != nil {
		fmt.Println("[!] Failed to start messenger:", err)
		return
	}
	defer messenger.Shutdown()

	// Handle Ctrl+C
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		fmt.Println("\n[*] Shutting down...")
		messenger.Shutdown()
		os.Exit(0)
	}()

	// --- Changed command list printing starts here ---
	fmt.Println("\nAvailable commands:")
	fmt.Println("  ╔════════════════════════════════════════════════════════════╗")
	fmt.Println("  ║ Command            │ Description                            ║")
	fmt.Println("  ╠════════════════════╪═══════════════════════════════════════╣")
	fmt.Println("  ║ add <name> <ip>    │ Add a contact                         ║")
	fmt.Println("  ║ [port]             │ (port optional, default 12345)       ║")
	fmt.Println("  ╠════════════════════╪═══════════════════════════════════════╣")
	fmt.Println("  ║ remove <name>      │ Remove a contact                      ║")
	fmt.Println("  ╠════════════════════╪═══════════════════════════════════════╣")
	fmt.Println("  ║ edit <name> <ip>   │ Edit a contact                        ║")
	fmt.Println("  ║ [port]             │ (port optional, default 12345)       ║")
	fmt.Println("  ╠════════════════════╪═══════════════════════════════════════╣")
	fmt.Println("  ║ list               │ List all contacts                     ║")
	fmt.Println("  ╠════════════════════╪═══════════════════════════════════════╣")
	fmt.Println("  ║ send <name> <msg>  │ Send a message                       ║")
	fmt.Println("  ╠════════════════════╪═══════════════════════════════════════╣")
	fmt.Println("  ║ exit               │ Quit the program                     ║")
	fmt.Println("  ╚════════════════════════════════════════════════════════════╝")
	// --- Changed command list printing ends here ---

	for {
		fmt.Print("\nCommand > ")
		input, err := reader.ReadString('\n')
		if err != nil {
			fmt.Println("[!] Error reading input:", err)
			continue
		}

		cmd := strings.Fields(strings.TrimSpace(input))
		if len(cmd) == 0 {
			continue
		}

		switch cmd[0] {
		case "exit":
			return

		case "add":
			if len(cmd) < 3 {
				fmt.Println("[!] Usage: add <name> <ip> [port]")
				continue
			}
			port := 12345
			if len(cmd) > 3 {
				p, err := strconv.Atoi(cmd[3])
				if err != nil {
					fmt.Println("[!] Invalid port number")
					continue
				}
				port = p
			}
			err := messenger.AddContact(cmd[1], cmd[2], port)
			if err != nil {
				fmt.Println("[!] Error:", err)
			}

		case "remove":
			if len(cmd) < 2 {
				fmt.Println("[!] Usage: remove <name>")
				continue
			}
			err := messenger.RemoveContact(cmd[1])
			if err != nil {
				fmt.Println("[!] Error:", err)
			}

		case "edit":
			if len(cmd) < 3 {
				fmt.Println("[!] Usage: edit <name> <ip> [port]")
				continue
			}
			port := 12345
			if len(cmd) > 3 {
				p, err := strconv.Atoi(cmd[3])
				if err != nil {
					fmt.Println("[!] Invalid port number")
					continue
				}
				port = p
			}
			err := messenger.EditContact(cmd[1], cmd[2], port)
			if err != nil {
				fmt.Println("[!] Error:", err)
			}

		case "list":
			messenger.ListContacts()

		case "send":
			if len(cmd) < 3 {
				fmt.Println("[!] Usage: send <name> <message>")
				continue
			}
			message := strings.Join(cmd[2:], " ")
			err := messenger.SendMessage(cmd[1], message)
			if err != nil {
				fmt.Println("[!] Error:", err)
			}

		default:
			fmt.Println("[!] Unknown command")
		}
	}
}
