package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

type responseCounts struct {
	Accepted   int64
	Succeeded  int64
	Rejected   int64
	RateLimit  int64
	SoldOut    int64
	QueueFull  int64
	Conflict   int64
	OtherError int64
	ServerErr  int64
}

func main() {
	base := flag.String("base", "http://127.0.0.1:8080", "API base URL")
	n := flag.Int("n", 1000, "number of requests")
	concurrency := flag.Int("c", 200, "concurrency")
	itemID := flag.Int64("item", 1, "item id")
	activityID := flag.Int64("activity", 1, "activity id for stock verification")
	quantity := flag.Int("qty", 1, "quantity per request")
	mode := flag.String("mode", "unique", "unique|duplicate")
	userID := flag.Int64("user", 900001, "user id for duplicate mode")
	key := flag.String("key", "load-duplicate-key", "idempotency key for duplicate mode")
	poll := flag.Bool("poll", true, "poll request status")
	timeout := flag.Duration("timeout", 60*time.Second, "overall timeout")
	dsn := flag.String("dsn", "", "optional MySQL DSN for read-only verification")
	adminToken := flag.String("admin-token", "", "optional admin token for stock verification")
	flag.Parse()

	if *n <= 0 || *concurrency <= 0 || *itemID <= 0 {
		fmt.Fprintln(os.Stderr, "n, c and item must be positive")
		os.Exit(2)
	}
	if *concurrency > *n {
		*concurrency = *n
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	client := &http.Client{Timeout: 10 * time.Second}
	var counts responseCounts
	var runKey = time.Now().UnixNano()

	jobs := make(chan int)
	var wg sync.WaitGroup
	for i := 0; i < *concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				uid := *userID
				idem := *key
				if *mode == "unique" {
					uid = int64(900000000 + idx + 1)
					idem = fmt.Sprintf("load-%d-%d", runKey, idx)
				}
				status, code, body := postOrder(ctx, client, *base, uid, idem, *itemID, *quantity)
				switch {
				case status == http.StatusAccepted:
					atomic.AddInt64(&counts.Accepted, 1)
				case status == http.StatusOK && code == "REJECTED":
					atomic.AddInt64(&counts.Rejected, 1)
				case status == http.StatusOK:
					atomic.AddInt64(&counts.Succeeded, 1)
				case status == http.StatusConflict && code == "SOLD_OUT":
					atomic.AddInt64(&counts.SoldOut, 1)
				case status == http.StatusConflict && code == "IDEMPOTENCY_CONFLICT":
					atomic.AddInt64(&counts.Conflict, 1)
				case status == http.StatusTooManyRequests && code == "QUEUE_FULL":
					atomic.AddInt64(&counts.QueueFull, 1)
				case status == http.StatusTooManyRequests:
					atomic.AddInt64(&counts.RateLimit, 1)
				case status >= 500:
					atomic.AddInt64(&counts.ServerErr, 1)
					fmt.Fprintf(os.Stderr, "server error status=%d body=%s\n", status, truncate(body))
				default:
					atomic.AddInt64(&counts.OtherError, 1)
					fmt.Fprintf(os.Stderr, "unexpected status=%d code=%s body=%s\n", status, code, truncate(body))
				}
				if *poll && status == http.StatusAccepted {
					pollStatus(ctx, client, *base, uid, idem)
				}
			}
		}()
	}
	for i := 0; i < *n; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	fmt.Printf("mode=%s requests=%d concurrency=%d item=%d qty=%d\n", *mode, *n, *concurrency, *itemID, *quantity)
	fmt.Printf("accepted=%d succeeded=%d rejected=%d rate_limited=%d sold_out=%d queue_full=%d conflict=%d other=%d server_error=%d\n",
		counts.Accepted, counts.Succeeded, counts.Rejected, counts.RateLimit, counts.SoldOut,
		counts.QueueFull, counts.Conflict, counts.OtherError, counts.ServerErr)

	if *dsn != "" {
		verifyDB(*dsn, *itemID, *userID, *mode)
	} else if *adminToken != "" {
		verifyStock(ctx, client, *base, *activityID, *adminToken)
	}
}

func postOrder(ctx context.Context, client *http.Client, base string, userID int64, key string, itemID int64, quantity int) (int, string, []byte) {
	body, _ := json.Marshal(map[string]interface{}{"itemId": itemID, "quantity": quantity})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/seckill/orders", bytes.NewReader(body))
	if err != nil {
		return 0, "", []byte(err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-Id", fmt.Sprint(userID))
	req.Header.Set("Idempotency-Key", key)
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", []byte(err.Error())
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var payload struct {
		Code   string `json:"code"`
		Status string `json:"status"`
	}
	_ = json.Unmarshal(data, &payload)
	code := payload.Code
	if code == "" && payload.Status == "REJECTED" {
		code = "REJECTED"
	}
	return resp.StatusCode, code, data
}

func pollStatus(ctx context.Context, client *http.Client, base string, userID int64, key string) (int, []byte) {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/seckill/requests/"+url.PathEscape(key), nil)
		if err != nil {
			return 0, nil
		}
		req.Header.Set("X-User-Id", fmt.Sprint(userID))
		resp, err := client.Do(req)
		if err != nil {
			return 0, nil
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			return resp.StatusCode, data
		}
		select {
		case <-ctx.Done():
			return 0, nil
		case <-time.After(200 * time.Millisecond):
		}
	}
	return 0, nil
}

func verifyDB(dsn string, itemID, userID int64, mode string) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verify db open: %v\n", err)
		return
	}
	defer db.Close()
	var orders int64
	if mode == "duplicate" {
		_ = db.QueryRow(`SELECT COUNT(*) FROM orders WHERE item_id = ? AND user_id = ?`, itemID, userID).Scan(&orders)
	} else {
		_ = db.QueryRow(`SELECT COUNT(*) FROM orders WHERE item_id = ?`, itemID).Scan(&orders)
	}
	var available, total int64
	_ = db.QueryRow(`SELECT available_stock, total_stock FROM seckill_items WHERE id = ?`, itemID).Scan(&available, &total)
	fmt.Printf("db_orders=%d available_stock=%d total_stock=%d\n", orders, available, total)
	if available < 0 {
		fmt.Fprintln(os.Stderr, "ASSERT FAILED: negative stock")
		os.Exit(1)
	}
	if orders > total {
		fmt.Fprintln(os.Stderr, "ASSERT FAILED: orders exceed stock")
		os.Exit(1)
	}
	if mode == "duplicate" && orders != 1 {
		fmt.Fprintln(os.Stderr, "ASSERT FAILED: duplicate mode must create exactly one order")
		os.Exit(1)
	}
}

func verifyStock(ctx context.Context, client *http.Client, base string, activityID int64, token string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/api/v1/admin/activities/%d/stock?includeRedis=true", base, activityID), nil)
	if err != nil {
		return
	}
	req.Header.Set("X-Admin-Token", token)
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verify stock: %v\n", err)
		return
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	fmt.Printf("stock_status=%d body=%s\n", resp.StatusCode, truncate(data))
}

func truncate(b []byte) string {
	if len(b) > 300 {
		return string(b[:300]) + "..."
	}
	return string(b)
}
