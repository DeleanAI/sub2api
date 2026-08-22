package repository

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
)

// fakeSentinel 是一个只会回答 go-redis 主节点发现流程的最小 RESP 服务：
// HELLO 按"旧服务器"回 unknown command（go-redis 据此退回 RESP2），
// SENTINEL GET-MASTER-ADDR-BY-NAME 回 miniredis 的地址，其余发现/订阅命令回空结果。
// 它记录收到的 AUTH 密码，用来证明哨兵连接用的是 sentinel_password 而不是数据节点密码。
type fakeSentinel struct {
	listener   net.Listener
	masterName string
	masterHost string
	masterPort string

	mu        sync.Mutex
	authSeen  []string
	connCount int
}

func newFakeSentinel(t *testing.T, masterName, masterAddr string) *fakeSentinel {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	host, port, err := net.SplitHostPort(masterAddr)
	require.NoError(t, err)
	s := &fakeSentinel{listener: listener, masterName: masterName, masterHost: host, masterPort: port}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.connCount++
			s.mu.Unlock()
			go s.serve(conn)
		}
	}()
	return s
}

func (s *fakeSentinel) addr() string { return s.listener.Addr().String() }

func (s *fakeSentinel) auths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.authSeen...)
}

func (s *fakeSentinel) connections() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connCount
}

// readCommand 解析一条 RESP 数组形式的命令（go-redis 发出的命令都是这种形式）。
func readCommand(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "*") {
		return nil, fmt.Errorf("unexpected RESP prefix %q", line)
	}
	count, err := strconv.Atoi(line[1:])
	if err != nil {
		return nil, err
	}
	args := make([]string, 0, count)
	for i := 0; i < count; i++ {
		header, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		size, err := strconv.Atoi(strings.TrimRight(header, "\r\n")[1:])
		if err != nil {
			return nil, err
		}
		buf := make([]byte, size+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		args = append(args, string(buf[:size]))
	}
	return args, nil
}

func bulk(values ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(values))
	for _, v := range values {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(v), v)
	}
	return b.String()
}

func (s *fakeSentinel) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	for {
		args, err := readCommand(r)
		if err != nil {
			return
		}
		var reply string
		switch strings.ToUpper(args[0]) {
		case "HELLO":
			reply = "-ERR unknown command 'HELLO'\r\n"
		case "AUTH":
			s.mu.Lock()
			s.authSeen = append(s.authSeen, args[len(args)-1])
			s.mu.Unlock()
			reply = "+OK\r\n"
		case "CLIENT", "PING":
			reply = "+OK\r\n"
		case "SENTINEL":
			switch {
			case len(args) >= 3 && strings.EqualFold(args[1], "GET-MASTER-ADDR-BY-NAME") && args[2] == s.masterName:
				reply = bulk(s.masterHost, s.masterPort)
			case len(args) >= 3 && strings.EqualFold(args[1], "GET-MASTER-ADDR-BY-NAME"):
				reply = "$-1\r\n"
			default: // SENTINELS / REPLICAS / SLAVES：没有别的节点
				reply = "*0\r\n"
			}
		case "SUBSCRIBE":
			var b strings.Builder
			for i, channel := range args[1:] {
				fmt.Fprintf(&b, "*3\r\n$9\r\nsubscribe\r\n$%d\r\n%s\r\n:%d\r\n", len(channel), channel, i+1)
			}
			reply = b.String()
		default:
			reply = "-ERR unknown command '" + args[0] + "'\r\n"
		}
		if _, err := io.WriteString(conn, reply); err != nil {
			return
		}
	}
}

// TestNewRedisClientSentinelModeDiscoversMaster 端到端验证 Sentinel 模式：客户端向哨兵询问主节点、
// 用数据节点的密码和 DB 连上 miniredis，并且哨兵连接用的是独立的 sentinel_password。
func TestNewRedisClientSentinelModeDiscoversMaster(t *testing.T) {
	master := miniredis.RunT(t)
	master.RequireAuth("data-pw")
	sentinel := newFakeSentinel(t, "mymaster", master.Addr())

	client := NewRedisClient(&config.RedisConfig{
		Host:             "ignored.invalid", // 配置了也不用：主节点地址来自哨兵
		Port:             1,
		Password:         "data-pw",
		DB:               1,
		SentinelAddrs:    []string{sentinel.addr()},
		MasterName:       "mymaster",
		SentinelPassword: "sentinel-pw",
		PoolSize:         4,
	})
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, client.Set(ctx, "k", "v", 0).Err())
	require.Equal(t, "v", mustGet(t, master, 1, "k"))

	require.GreaterOrEqual(t, sentinel.connections(), 1, "master address must have been resolved through the sentinel")
	for _, pw := range sentinel.auths() {
		require.Equal(t, "sentinel-pw", pw, "sentinel connections must authenticate with sentinel_password, not the data-node password")
	}
	require.NotEmpty(t, sentinel.auths(), "sentinel connections must authenticate when sentinel_password is set")
}
