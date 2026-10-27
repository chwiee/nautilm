// Package secrets resolves the database connection URL.
//
//	database: {url: ${NAUTILM_DATABASE_URL}}               # Kubernetes Secret → env (local cluster)
//	database: {aws_secret_arn: arn:aws:secretsmanager:...}  # AWS Secrets Manager (EKS)
//
// With Secrets Manager, nautilm reads the secret itself with its Pod
// Identity role: no CSI driver or External Secrets operator to install, only
// secretsmanager:GetSecretValue on that one secret. The secret holds the URL
// as plain text or as JSON ({"url": "postgres://..."}).
package secrets

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

// GetSecretValueAPI is the part of the SDK used (testable).
type GetSecretValueAPI interface {
	GetSecretValue(ctx context.Context, in *secretsmanager.GetSecretValueInput, opts ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
}

// DatabaseURL returns the URL from the config or from Secrets Manager.
func DatabaseURL(ctx context.Context, url, arn, jsonKey string, api GetSecretValueAPI) (string, error) {
	if url != "" {
		return url, nil
	}
	if arn == "" {
		return "", fmt.Errorf("database: no url and no aws_secret_arn")
	}
	if api == nil {
		region, err := regionOf(arn)
		if err != nil {
			return "", err
		}
		cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
		if err != nil {
			return "", fmt.Errorf("aws credentials: %w", err)
		}
		api = secretsmanager.NewFromConfig(cfg)
	}
	out, err := api.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(arn)})
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", arn, err)
	}
	value := strings.TrimSpace(aws.ToString(out.SecretString))
	if strings.HasPrefix(value, "{") {
		var m map[string]string
		if err := json.Unmarshal([]byte(value), &m); err != nil {
			return "", fmt.Errorf("secret %s is JSON but not a flat object of strings", arn)
		}
		v, ok := m[jsonKey]
		if !ok || v == "" {
			return "", fmt.Errorf("secret %s has no key %q (set database.aws_secret_key)", arn, jsonKey)
		}
		return v, nil
	}
	if value == "" {
		return "", fmt.Errorf("secret %s is empty", arn)
	}
	return value, nil
}

// AWSReader reads secrets with the pod's own AWS credentials (Pod Identity),
// one client per Region (taken from the ARN). AWS_ENDPOINT_URL works too, so
// a local emulator can stand in for Secrets Manager in the lab.
type AWSReader struct {
	mu      sync.Mutex
	clients map[string]GetSecretValueAPI
}

// Read returns the secret's text.
func (a *AWSReader) Read(ctx context.Context, arn string) (string, error) {
	region, err := regionOf(arn)
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	api, ok := a.clients[region]
	if !ok {
		cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
		if err != nil {
			a.mu.Unlock()
			return "", fmt.Errorf("aws credentials: %w", err)
		}
		api = secretsmanager.NewFromConfig(cfg)
		if a.clients == nil {
			a.clients = map[string]GetSecretValueAPI{}
		}
		a.clients[region] = api
	}
	a.mu.Unlock()
	out, err := api.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(arn)})
	if err != nil {
		return "", err // kept as is: the wizard explains the AWS error code
	}
	if out.SecretString == nil {
		return "", fmt.Errorf("secret %s has no text value (binary secrets are not supported)", arn)
	}
	return *out.SecretString, nil
}

// regionOf extracts the Region from arn:aws:secretsmanager:<region>:<account>:secret:<name>.
func regionOf(arn string) (string, error) {
	parts := strings.Split(arn, ":")
	if len(parts) < 7 || parts[2] != "secretsmanager" || parts[3] == "" {
		return "", fmt.Errorf("%q is not a Secrets Manager ARN", arn)
	}
	return parts[3], nil
}
