# Starts nautilm locally against the real Bedrock (us-east-2) using the
# "lab" profile from `aws login --profile lab`. Test-only tokens below: they
# exist only on this machine. Run from the repository root:
#
#   & .\examples\local-aws\run.ps1
$env:AWS_PROFILE = 'lab'
$env:AWS_REGION = 'us-east-2'
$env:NAUTILM_TOKEN_K8S = 'local-k8s'
$env:NAUTILM_TOKEN_MID = 'local-mid'
$env:NAUTILM_TOKEN_MARCIA = 'local-marcia'
$env:NAUTILM_TOKEN_CLAUDIA = 'local-claudia'
$env:NAUTILM_DASHBOARD_TOKEN = 'local-dashboard'
go run ./cmd/nautilm -config examples/local-aws/nautilm.yaml
