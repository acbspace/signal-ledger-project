package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config contains environment-driven settings shared by the Go services.
// Database and quant-service URLs are present now so service wiring is explicit;
// their clients will be added in the next implementation milestones.
type Config struct {
	HTTPAddr            string
	DatabaseURL         string
	DocumentStoragePath string
	MaxUploadBytes      int64
	QuantServiceURL     string
	QuantTimeout        time.Duration
	WorkerID            string
	WorkerPollInterval  time.Duration
	JobLeaseDuration    time.Duration
}

func Load() (Config, error) {
	pollInterval, err := time.ParseDuration(value("WORKER_POLL_INTERVAL", "5s"))
	if err != nil {
		return Config{}, fmt.Errorf("parse WORKER_POLL_INTERVAL: %w", err)
	}
	// LLM extraction of a long research PDF can take minutes, so the quant
	// call budget and the job lease must both outlast a full document pass.
	leaseDuration, err := time.ParseDuration(value("JOB_LEASE_DURATION", "10m"))
	if err != nil {
		return Config{}, fmt.Errorf("parse JOB_LEASE_DURATION: %w", err)
	}
	quantTimeout, err := time.ParseDuration(value("QUANT_TIMEOUT", "300s"))
	if err != nil {
		return Config{}, fmt.Errorf("parse QUANT_TIMEOUT: %w", err)
	}
	maxUploadBytes, err := strconv.ParseInt(value("MAX_UPLOAD_BYTES", "20971520"), 10, 64)
	if err != nil || maxUploadBytes <= 0 {
		return Config{}, fmt.Errorf("parse MAX_UPLOAD_BYTES: must be a positive integer")
	}

	defaultWorkerID, err := os.Hostname()
	if err != nil || defaultWorkerID == "" {
		defaultWorkerID = "worker"
	}

	return Config{
		HTTPAddr:            value("HTTP_ADDR", ":8080"),
		DatabaseURL:         value("DATABASE_URL", "postgres://signalledger:signalledger@postgres:5432/signalledger?sslmode=disable"),
		DocumentStoragePath: value("DOCUMENT_STORAGE_PATH", "/var/lib/signalledger/documents"),
		MaxUploadBytes:      maxUploadBytes,
		QuantServiceURL:     value("QUANT_SERVICE_URL", "http://quant:8000"),
		QuantTimeout:        quantTimeout,
		WorkerID:            value("WORKER_ID", defaultWorkerID),
		WorkerPollInterval:  pollInterval,
		JobLeaseDuration:    leaseDuration,
	}, nil
}

func value(key, fallback string) string {
	if result, ok := os.LookupEnv(key); ok && result != "" {
		return result
	}
	return fallback
}
