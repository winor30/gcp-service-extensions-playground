package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/lestrrat-go/jwx/v3/jwa"
	jwxjwt "github.com/lestrrat-go/jwx/v3/jwt"
)

type config struct {
	TargetURL      string
	PrivateKeyFile string
	Subject        string
}

type runConfig struct {
	WarmupRuns  int
	MeasureRuns int
}

type scenario struct {
	name      string
	authValue string
}

func parseConfig() (config, error) {
	cfg := config{
		TargetURL:      os.Getenv("TARGET_URL"),
		PrivateKeyFile: os.Getenv("PRIVATE_KEY_PEM_FILE"),
		Subject:        os.Getenv("JWT_SUB"),
	}
	if cfg.TargetURL == "" {
		return config{}, errors.New("target URL is required")
	}
	if cfg.PrivateKeyFile == "" {
		return config{}, errors.New("private key file is required")
	}
	if cfg.Subject == "" {
		return config{}, errors.New("subject is required")
	}
	return cfg, nil
}

func parseRunConfig() runConfig {
	warmupRuns := 5
	measureRuns := 10

	if raw := os.Getenv("WARMUP_RUNS"); raw != "" {
		if value, err := strconv.Atoi(raw); err == nil && value >= 0 {
			warmupRuns = value
		}
	}
	if raw := os.Getenv("MEASURE_RUNS"); raw != "" {
		if value, err := strconv.Atoi(raw); err == nil && value > 0 {
			measureRuns = value
		}
	}
	return runConfig{WarmupRuns: warmupRuns, MeasureRuns: measureRuns}
}

func buildRS256Token(privateKey *rsa.PrivateKey, subject string, now time.Time) (string, error) {
	builder := jwxjwt.NewBuilder().
		IssuedAt(now).
		Expiration(now.Add(5 * time.Minute))
	if subject != "" {
		builder = builder.Subject(subject)
	}
	token, err := builder.Build()
	if err != nil {
		return "", fmt.Errorf("build token: %w", err)
	}
	signed, err := jwxjwt.Sign(token, jwxjwt.WithKey(jwa.RS256(), privateKey))
	if err != nil {
		return "", fmt.Errorf("sign token: %w", err)
	}
	return string(signed), nil
}

func doRequest(httpClient *http.Client, targetURL, authValue string) (int, string, time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return 0, "", 0, err
	}
	if authValue != "" {
		req.Header.Set("Authorization", authValue)
	}
	resp, err := httpClient.Do(req)
	latency := time.Since(start)
	if err != nil {
		return 0, "", latency, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, "", latency, err
	}
	return resp.StatusCode, string(body), latency, nil
}

func summarizeLatencies(latencies []time.Duration) (time.Duration, time.Duration, time.Duration, time.Duration) {
	if len(latencies) == 0 {
		return 0, 0, 0, 0
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	min := latencies[0]
	max := latencies[len(latencies)-1]

	var total int64
	for _, d := range latencies {
		total += d.Nanoseconds()
	}
	avg := time.Duration(total / int64(len(latencies)))

	mid := len(latencies) / 2
	median := latencies[mid]
	if len(latencies)%2 == 0 {
		median = (latencies[mid-1] + latencies[mid]) / 2
	}
	return avg, median, min, max
}

func buildHS256Token(subject string, now time.Time) (string, error) {
	builder := jwxjwt.NewBuilder().
		IssuedAt(now).
		Expiration(now.Add(5 * time.Minute))
	if subject != "" {
		builder = builder.Subject(subject)
	}
	token, err := builder.Build()
	if err != nil {
		return "", fmt.Errorf("build token: %w", err)
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", fmt.Errorf("read secret: %w", err)
	}
	signed, err := jwxjwt.Sign(token, jwxjwt.WithKey(jwa.HS256(), secret))
	if err != nil {
		return "", fmt.Errorf("sign token: %w", err)
	}
	return string(signed), nil
}

func parseRSAPrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("invalid PEM data")
	}

	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse PKCS1 private key: %w", err)
		}
		return key, nil
	case "PRIVATE KEY":
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse PKCS8 private key: %w", err)
		}
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("invalid key type")
		}
		return rsaKey, nil
	default:
		return nil, fmt.Errorf("unsupported PEM type: %s", block.Type)
	}
}

func main() {
	cfg, err := parseConfig()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	privateKeyPEM, err := os.ReadFile(cfg.PrivateKeyFile)
	if err != nil {
		log.Fatalf("read private key error: %v", err)
	}
	privateKey, err := parseRSAPrivateKey(privateKeyPEM)
	if err != nil {
		log.Fatalf("read private key error: %v", err)
	}

	runCfg := parseRunConfig()
	now := time.Now()
	validToken, err := buildRS256Token(privateKey, cfg.Subject, now)
	if err != nil {
		log.Fatalf("build valid token error: %v", err)
	}

	noSubToken, err := buildRS256Token(privateKey, "", now)
	if err != nil {
		log.Fatalf("build no-sub token error: %v", err)
	}

	altKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		log.Fatalf("generate alt key error: %v", err)
	}
	badSigToken, err := buildRS256Token(altKey, cfg.Subject, now)
	if err != nil {
		log.Fatalf("build bad-signature token error: %v", err)
	}

	wrongAlgToken, err := buildHS256Token(cfg.Subject, now)
	if err != nil {
		log.Fatalf("build wrong-alg token error: %v", err)
	}

	scenarios := []scenario{
		{name: "valid", authValue: "Bearer " + validToken},
		{name: "missing_auth", authValue: ""},
		{name: "not_bearer", authValue: "Token " + validToken},
		{name: "malformed", authValue: "Bearer abc.def"},
		{name: "missing_sub", authValue: "Bearer " + noSubToken},
		{name: "bad_signature", authValue: "Bearer " + badSigToken},
		{name: "wrong_alg", authValue: "Bearer " + wrongAlgToken},
	}

	httpClient := &http.Client{Timeout: 10 * time.Second}
	for _, sc := range scenarios {
		for i := 0; i < runCfg.WarmupRuns; i++ {
			_, _, _, _ = doRequest(httpClient, cfg.TargetURL, sc.authValue)
		}

		var latencies []time.Duration
		statusCounts := make(map[int]int)
		var sampleBody string
		errorCount := 0

		for i := 0; i < runCfg.MeasureRuns; i++ {
			status, body, latency, err := doRequest(httpClient, cfg.TargetURL, sc.authValue)
			if err != nil {
				errorCount++
				continue
			}
			if sampleBody == "" {
				sampleBody = body
			}
			latencies = append(latencies, latency)
			statusCounts[status]++
		}

		avg, median, min, max := summarizeLatencies(latencies)
		fmt.Printf("case=%s warmup=%d runs=%d errors=%d\n", sc.name, runCfg.WarmupRuns, runCfg.MeasureRuns, errorCount)
		if len(statusCounts) == 0 {
			fmt.Printf("case=%s status_counts=none\n", sc.name)
		} else {
			keys := make([]int, 0, len(statusCounts))
			for status := range statusCounts {
				keys = append(keys, status)
			}
			sort.Ints(keys)
			fmt.Printf("case=%s status_counts=", sc.name)
			for i, status := range keys {
				if i > 0 {
					fmt.Printf(",")
				}
				fmt.Printf("%d:%d", status, statusCounts[status])
			}
			fmt.Printf("\n")
		}
		fmt.Printf("case=%s latency_avg=%s latency_median=%s latency_min=%s latency_max=%s\n", sc.name, avg, median, min, max)
		fmt.Printf("case=%s body=%s\n", sc.name, sampleBody)
	}
}
