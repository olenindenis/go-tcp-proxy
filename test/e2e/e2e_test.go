//go:build e2e

package e2e

import (
	"bufio"
	"cmp"
	"database/sql"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/gofiber/fiber/v3/client"
)

var (
	mysqlAddr = cmp.Or(os.Getenv("E2E_MYSQL_ADDR"), "127.0.0.1:3307")
	redisAddr = cmp.Or(os.Getenv("E2E_REDIS_ADDR"), "127.0.0.1:6380")
	adminAddr = cmp.Or(os.Getenv("E2E_ADMIN_ADDR"), "127.0.0.1:9090")
)

func TestMySQLThroughProxy(t *testing.T) {
	db, err := sql.Open("mysql", "root:root@tcp("+mysqlAddr+")/?timeout=5s")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(10)

	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			var v int
			if err := db.QueryRowContext(t.Context(), "SELECT SLEEP(0.2) + 42").Scan(&v); err != nil {
				t.Error(err)
				return
			}
			if v != 42 {
				t.Errorf("got %d", v)
			}
		})
	}
	wg.Wait()
}

func TestRedisThroughProxy(t *testing.T) {
	c, err := net.DialTimeout("tcp", redisAddr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte("PING\r\n")); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if line != "+PONG\r\n" {
		t.Fatalf("got %q", line)
	}
}

func TestMetricsExposed(t *testing.T) {
	resp, err := client.Get("http://" + adminAddr + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Close()
	body := resp.Body()
	for _, want := range []string{
		`tcp_proxy_connections_accepted_total{route="mysql"}`,
		`tcp_proxy_bytes_transferred_total{direction="upstream_to_client",route="redis"}`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("metric %s not found", want)
		}
	}
}
