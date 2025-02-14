package main

import (
    "bytes"
    "flag"
    "fmt"
    "io"
    "log"
    "net"
    "strings"
    "sync"
    "time"
)

type ProxyConfig struct {
    ListenAddr      string
    UpstreamAddr    string
    HeadersToModify map[string]string
    KeywordsReplace [][2]string
    HeadersToRemove []string
    Timeout         time.Duration
}

func processHeaders(data []byte, config *ProxyConfig) []byte {
    // 快速检查是否为HTTP请求
    if !bytes.Contains(data, []byte("HTTP/")) {
        return data
    }

    // 使用更高效的 bytes.Buffer
    var buf bytes.Buffer
    buf.Grow(len(data))

    // 处理请求行和请求头
    lines := bytes.Split(data, []byte("\r\n"))
    if len(lines) > 0 {
        buf.Write(lines[0]) // 保留请求行
        buf.WriteString("\r\n")
    }

    // 处理其余请求头
    for _, line := range lines[1:] {
        if len(line) == 0 {
            continue
        }

        // 检查是否需要移除
        shouldRemove := false
        for _, headerToRemove := range config.HeadersToRemove {
            if bytes.HasPrefix(bytes.ToLower(line), bytes.ToLower([]byte(headerToRemove+":"))) {
                shouldRemove = true
                break
            }
        }
        if shouldRemove {
            continue
        }

        // 关键词替换
        lineStr := string(line)
        for _, kv := range config.KeywordsReplace {
            lineStr = strings.ReplaceAll(lineStr, kv[0], kv[1])
        }

        // 请求头修改
        modified := false
        for oldHeader, newHeader := range config.HeadersToModify {
            if strings.HasPrefix(strings.ToLower(lineStr), strings.ToLower(oldHeader)+":") {
                parts := strings.SplitN(lineStr, ":", 2)
                if len(parts) == 2 {
                    buf.WriteString(oldHeader)
                    buf.WriteString(":")
                    buf.WriteString(newHeader)
                    buf.WriteString("\r\n")
                    modified = true
                    break
                }
            }
        }

        if !modified {
            buf.WriteString(lineStr)
            buf.WriteString("\r\n")
        }
    }

    return buf.Bytes()
}

func copyData(dst net.Conn, src net.Conn, config *ProxyConfig, processFunc func([]byte, *ProxyConfig) []byte) error {
    buffer := make([]byte, 64*1024) // 64KB buffer
    remainder := make([]byte, 0, 64*1024) // 用于存储不完整的HTTP请求
    
    for {
        src.SetReadDeadline(time.Now().Add(config.Timeout))
        n, err := src.Read(buffer)
        if err != nil {
            if err == io.EOF {
                return nil
            }
            if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
                continue
            }
            return fmt.Errorf("读取错误: %v", err)
        }

        if n > 0 {
            var data []byte
            if processFunc != nil {
                // 将新读取的数据追加到之前的余数据
                fullData := append(remainder, buffer[:n]...)
                
                // 寻找请求分界点
                requests := bytes.Split(fullData, []byte("\r\n\r\n"))
                
                var processedData bytes.Buffer
                processedData.Grow(len(fullData))
                
                // 处理所有完整的请求
                for i := 0; i < len(requests)-1; i++ {
                    processedRequest := processHeaders(append(requests[i], []byte("\r\n\r\n")...), config)
                    processedData.Write(processedRequest)
                }
                
                // 保存最后一段不完整的数据
                if len(requests) > 0 {
                    remainder = requests[len(requests)-1]
                    // 如果发现了完整的请求结束标记，处理最后一段
                    if bytes.HasSuffix(fullData, []byte("\r\n\r\n")) {
                        processedRequest := processHeaders(append(remainder, []byte("\r\n\r\n")...), config)
                        processedData.Write(processedRequest)
                        remainder = nil
                    }
                }
                
                data = processedData.Bytes()
            } else {
                data = buffer[:n]
            }

            dst.SetWriteDeadline(time.Now().Add(config.Timeout))
            _, err := dst.Write(data)
            if err != nil {
                return fmt.Errorf("写入错误: %v", err)
            }
        }
    }
}

func parseKeywords(keywords string) [][2]string {
    var result [][2]string
    if keywords == "" {
        return result
    }

    pairs := strings.Split(keywords, ",")
    for _, pair := range pairs {
        parts := strings.Split(pair, "=>")
        if len(parts) == 2 {
            oldVal := strings.TrimSpace(parts[0])
            newVal := strings.TrimSpace(parts[1])
            if oldVal != "" && newVal != "" {
                result = append(result, [2]string{oldVal, newVal})
            }
        }
    }
    return result
}

func handleConnection(clientConn net.Conn, config *ProxyConfig) {
    defer clientConn.Close()
    
    // 连接上游服务器
    upstreamConn, err := net.DialTimeout("tcp", config.UpstreamAddr, config.Timeout)
    if err != nil {
        log.Printf("无法连接到上游服务器 %s: %v", config.UpstreamAddr, err)
        return
    }
    defer upstreamConn.Close()

    var wg sync.WaitGroup
    wg.Add(2)

    // 客户端 -> 上游
    go func() {
        defer wg.Done()
        if err := copyData(upstreamConn, clientConn, config, processHeaders); err != nil {
            if !strings.Contains(err.Error(), "use of closed network connection") {
                log.Printf("客户端->上游错误: %v", err)
            }
        }
    }()

    // 上游 -> 客户端
    go func() {
        defer wg.Done()
        if err := copyData(clientConn, upstreamConn, config, nil); err != nil {
            if !strings.Contains(err.Error(), "use of closed network connection") {
                log.Printf("上游->客户端错误: %v", err)
            }
        }
    }()

    wg.Wait()
}

func main() {
    listenAddr := flag.String("listen", ":8080", "监听地址:端口")
    upstreamAddr := flag.String("upstream", "localhost:80", "上游服务器地址:端口")
    headers := flag.String("headers", "", "要修改的请求头 (格式: 'Header1:Value1,Header2:Value2')")
    keywords := flag.String("keywords", "", "关键词替换 (格式: 'old1=>new1,old2=>new2')")
    removeHeaders := flag.String("remove-headers", "", "要移除的请求头 (用逗号分隔)")
    timeout := flag.Duration("timeout", 60*time.Second, "连接超时时间")
    flag.Parse()

    headersToModify := make(map[string]string)
    if *headers != "" {
        headerPairs := strings.Split(*headers, ",")
        for _, pair := range headerPairs {
            parts := strings.SplitN(pair, ":", 2)
            if len(parts) == 2 {
                headersToModify[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
            }
        }
    }
    
    var headersToRemove []string
    if *removeHeaders != "" {
        for _, header := range strings.Split(*removeHeaders, ",") {
            header = strings.TrimSpace(header)
            if header != "" {
                headersToRemove = append(headersToRemove, header)
            }
        }
    }

    config := &ProxyConfig{
        ListenAddr:      *listenAddr,
        UpstreamAddr:    *upstreamAddr,
        HeadersToModify: headersToModify,
        KeywordsReplace: parseKeywords(*keywords),
        HeadersToRemove: headersToRemove,
        Timeout:         *timeout,
    }

    listener, err := net.Listen("tcp", config.ListenAddr)
    if err != nil {
        log.Fatalf("无法创建监听器: %v", err)
    }
    defer listener.Close()

    log.Printf("代理服务器正在监听 %s，转发到 %s", config.ListenAddr, config.UpstreamAddr)
    if len(config.KeywordsReplace) > 0 {
        log.Printf("已配置关键词替换规则:")
        for _, kv := range config.KeywordsReplace {
            log.Printf("  %s -> %s", kv[0], kv[1])
        }
    }
    if len(config.HeadersToRemove) > 0 {
        log.Printf("将移除以下请求头: %v", config.HeadersToRemove)
    }

    for {
        conn, err := listener.Accept()
        if err != nil {
            log.Printf("接受连接错误: %v", err)
            continue
        }
        
        go handleConnection(conn, config)
    }
}