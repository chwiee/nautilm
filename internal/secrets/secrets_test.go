package secrets

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

type fakeSM struct {
	value string
	err   error
	asked string
}

func (f *fakeSM) GetSecretValue(_ context.Context, in *secretsmanager.GetSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	f.asked = aws.ToString(in.SecretId)
	return &secretsmanager.GetSecretValueOutput{SecretString: aws.String(f.value)}, f.err
}

const arn = "arn:aws:secretsmanager:us-east-2:123456789012:secret:nautilm/db-AbCdEf"

func TestDatabaseURL(t *testing.T) {
	ctx := context.Background()
	if u, _ := DatabaseURL(ctx, "postgres://env", arn, "url", &fakeSM{}); u != "postgres://env" {
		t.Error("an explicit url wins")
	}
	plain := &fakeSM{value: "postgres://u:p@db:5432/nautilm"}
	if u, err := DatabaseURL(ctx, "", arn, "url", plain); err != nil || u != plain.value || plain.asked != arn {
		t.Errorf("plain text secret: %q %v", u, err)
	}
	js := &fakeSM{value: `{"url":"postgres://json","username":"x"}`}
	if u, _ := DatabaseURL(ctx, "", arn, "url", js); u != "postgres://json" {
		t.Errorf("json secret: %q", u)
	}
	if _, err := DatabaseURL(ctx, "", arn, "connection", js); err == nil || !strings.Contains(err.Error(), `no key "connection"`) {
		t.Errorf("missing key: %v", err)
	}
	if _, err := DatabaseURL(ctx, "", arn, "url", &fakeSM{err: errors.New("AccessDeniedException")}); err == nil {
		t.Error("access denied must surface")
	}
	if _, err := regionOf("arn:aws:s3:::bucket"); err == nil {
		t.Error("not a secrets manager arn")
	}
	if r, _ := regionOf(arn); r != "us-east-2" {
		t.Errorf("region: %s", r)
	}
}
