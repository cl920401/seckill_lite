package config

import (
	"os"
	"strconv"
	"time"
)

type Config struct {
	HTTPAddr       string
	MySQLDSN       string
	RedisAddr      string
	RedisPassword  string
	RedisDB        int
	RedisPrefix    string
	AdminToken     string
	IngressRPS     float64
	IngressBurst   int
	QueryRPS       float64
	QueryBurst     int
	UserRPS        float64
	UserBurst      int
	StreamKey      string
	StreamGroup    string
	StreamMax      int64
	ClaimIdle      time.Duration
	OrderExpire    time.Duration
	RequestTTL     time.Duration
	RestockTTL     time.Duration
	CancelInterval time.Duration
	CancelBatch    int
	HistoryLookups int
	Readiness      bool
}

func Load() Config {
	return Config{
		HTTPAddr:       env("HTTP_ADDR", ":8080"),
		MySQLDSN:       env("MYSQL_DSN", "root@tcp(127.0.0.1:3306)/seckill?parseTime=true&loc=UTC&charset=utf8mb4"),
		RedisAddr:      env("REDIS_ADDR", "127.0.0.1:6379"),
		RedisPassword:  os.Getenv("REDIS_PASSWORD"),
		RedisDB:        envInt("REDIS_DB", 0),
		RedisPrefix:    env("REDIS_PREFIX", "sk:"),
		AdminToken:     env("ADMIN_TOKEN", "demo-admin-token"),
		IngressRPS:     envFloat("INGRESS_GLOBAL_RPS", 1000),
		IngressBurst:   envInt("INGRESS_GLOBAL_BURST", 1000),
		QueryRPS:       envFloat("QUERY_GLOBAL_RPS", 2000),
		QueryBurst:     envInt("QUERY_GLOBAL_BURST", 2000),
		UserRPS:        envFloat("USER_NEW_ORDER_RPS", 5),
		UserBurst:      envInt("USER_NEW_ORDER_BURST", 10),
		StreamKey:      env("STREAM_KEY", "sk:stream:orders"),
		StreamGroup:    env("STREAM_GROUP", "order-workers"),
		StreamMax:      int64(envInt("STREAM_MAX_ENTRIES", 10000)),
		ClaimIdle:      envDuration("STREAM_CLAIM_IDLE", 60*time.Second),
		OrderExpire:    envDuration("ORDER_EXPIRE", 15*time.Minute),
		RequestTTL:     envDuration("REQUEST_STATE_TTL", 24*time.Hour),
		RestockTTL:     envDuration("RESTOCK_MARKER_TTL", 24*time.Hour),
		CancelInterval: envDuration("CANCEL_INTERVAL", time.Second),
		CancelBatch:    envInt("CANCEL_BATCH", 100),
		HistoryLookups: envInt("HISTORY_LOOKUP_CONCURRENCY", 32),
		Readiness:      envBool("READINESS_ENABLED", true),
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func envFloat(k string, def float64) float64 {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return n
}

func envDuration(k string, def time.Duration) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

func envBool(k string, def bool) bool {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}
