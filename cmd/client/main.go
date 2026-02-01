package main

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/lestrrat-go/jwx/v3/jwa"
	jwxjwt "github.com/lestrrat-go/jwx/v3/jwt"
)

type config struct {
	TargetURL      string
	PrivateKeyFile string
	Subject        string
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

	token, err := jwxjwt.NewBuilder().
		Subject(cfg.Subject).
		IssuedAt(time.Now()).
		Expiration(time.Now().Add(5 * time.Minute)).
		Build()
	if err != nil {
		log.Fatalf("build token error: %v", err)
	}

	signed, err := jwxjwt.Sign(token, jwxjwt.WithKey(jwa.RS256(), privateKey))
	if err != nil {
		log.Fatalf("sign token error: %v", err)
	}
	jwtString := string(signed)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	httpClient := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.TargetURL, nil)
	if err != nil {
		log.Fatalf("request build error: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwtString)

	resp, err := httpClient.Do(req)
	if err != nil {
		log.Fatalf("request error: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Fatalf("read body error: %v", err)
	}

	fmt.Printf("status: %d\n", resp.StatusCode)
	fmt.Printf("body: %s\n", string(body))
}
