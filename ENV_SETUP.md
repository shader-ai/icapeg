# Environment Variables Setup for ICAPeg

ICAPeg supports business logic features (tenant validation, URL matching, SQS) that require environment variables to be configured.

## Setup Options

### Option 1: Using a `.env` file (Recommended for local development)

1. Copy the example file:
   ```bash
   cp .env.example .env
   ```

2. Edit `.env` and fill in your values:
   ```bash
   # Database Configuration (PostgreSQL)
   DATABASE_URL=postgresql://user:password@localhost:5432/icapdb?sslmode=disable

   # AWS SQS Configuration (Optional)
   RECORDING_QUEUE_URL=https://sqs.us-east-1.amazonaws.com/123456789012/recording-queue
   AWS_REGION=us-east-1
   AWS_ACCESS_KEY_ID=your-access-key-id
   AWS_SECRET_ACCESS_KEY=your-secret-access-key
   ```

3. The `.env` file will be automatically loaded when you start the server.

**Note**: The `.env` file is gitignored and will not be committed to version control.

### Option 2: Setting environment variables directly

You can set environment variables directly in your shell or system:

#### Linux/macOS:
```bash
export DATABASE_URL="postgresql://user:password@localhost:5432/icapdb?sslmode=disable"
export RECORDING_QUEUE_URL="https://sqs.us-east-1.amazonaws.com/123456789012/recording-queue"
export AWS_REGION="us-east-1"
export AWS_ACCESS_KEY_ID="your-access-key-id"
export AWS_SECRET_ACCESS_KEY="your-secret-access-key"
```

#### Windows (PowerShell):
```powershell
$env:DATABASE_URL="postgresql://user:password@localhost:5432/icapdb?sslmode=disable"
$env:RECORDING_QUEUE_URL="https://sqs.us-east-1.amazonaws.com/123456789012/recording-queue"
$env:AWS_REGION="us-east-1"
$env:AWS_ACCESS_KEY_ID="your-access-key-id"
$env:AWS_SECRET_ACCESS_KEY="your-secret-access-key"
```

#### Windows (CMD):
```cmd
set DATABASE_URL=postgresql://user:password@localhost:5432/icapdb?sslmode=disable
set RECORDING_QUEUE_URL=https://sqs.us-east-1.amazonaws.com/123456789012/recording-queue
set AWS_REGION=us-east-1
set AWS_ACCESS_KEY_ID=your-access-key-id
set AWS_SECRET_ACCESS_KEY=your-secret-access-key
```

### Option 3: Using config.toml with `$_` prefix

ICAPeg also supports reading environment variables through the `config.toml` file using the `$_` prefix. However, for business logic configuration, it's recommended to use a `.env` file or set environment variables directly.

## Required Environment Variables

### Database Configuration

- **`DATABASE_URL`** (Required for tenant validation and URL matching)
  - Format: `postgresql://user:password@host:port/database?sslmode=disable`
  - Example: `postgresql://postgres:password@localhost:5432/icapdb?sslmode=disable`

### AWS SQS Configuration (Optional)

- **`RECORDING_QUEUE_URL`** (Optional)
  - SQS queue URL for recording requests
  - Format: `https://sqs.region.amazonaws.com/account-id/queue-name`
  - Example: `https://sqs.us-east-1.amazonaws.com/123456789012/recording-queue`

- **`AWS_REGION`** (Optional, default: `us-east-1`)
  - AWS region where your SQS queue is located

- **`AWS_ACCESS_KEY_ID`** (Optional)
  - AWS access key ID
  - Can be omitted if using IAM roles (recommended for production)

- **`AWS_SECRET_ACCESS_KEY`** (Optional)
  - AWS secret access key
  - Can be omitted if using IAM roles (recommended for production)

### Region (Optional)

- **`REGION_CODE`** (Optional)
  - Region code for this ICAP deployment (e.g. `us-east`, `eu`). Sent in the SQS recording message; the backend resolves it to a tenant region and stores `region_id` on request records and users for region-based filtering.
  - Set per deployment/location so each site identifies its region. If unset, recordings are stored without a region (tenant-only).

## Production Deployment

For production deployments:

1. **Use IAM roles** instead of access keys when running on AWS (EC2, ECS, Lambda)
2. **Set environment variables** in your deployment platform (Docker, Kubernetes, etc.)
3. **Do not commit** `.env` files to version control
4. **Use secrets management** services (AWS Secrets Manager, HashiCorp Vault, etc.)

### Docker Example

```dockerfile
# Dockerfile
ENV DATABASE_URL=${DATABASE_URL}
ENV RECORDING_QUEUE_URL=${RECORDING_QUEUE_URL}
ENV AWS_REGION=${AWS_REGION}
```

```bash
# docker-compose.yml or docker run
docker run -e DATABASE_URL="..." -e RECORDING_QUEUE_URL="..." icapeg
```

### Kubernetes Example

```yaml
# deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: icapeg
spec:
  template:
    spec:
      containers:
      - name: icapeg
        env:
        - name: DATABASE_URL
          valueFrom:
            secretKeyRef:
              name: icapeg-secrets
              key: database-url
        - name: RECORDING_QUEUE_URL
          valueFrom:
            secretKeyRef:
              name: icapeg-secrets
              key: sqs-queue-url
```

## Verification

After setting up environment variables, start the ICAPeg server:

```bash
./icapeg
```

Check the logs to verify:
- If database connection is successful: `Database connection established successfully`
- If SQS is configured: `Successfully enqueued recording request to SQS`
- If business logic is disabled: `Failed to initialize business logic: ... Continuing without business logic features.`

## Troubleshooting

### Database connection fails
- Verify `DATABASE_URL` is correct
- Check database is accessible from ICAPeg server
- Verify database credentials are correct
- Check PostgreSQL is running

### SQS errors
- Verify `RECORDING_QUEUE_URL` is correct
- Check AWS credentials have SQS permissions
- Verify queue exists in the specified region
- Check network connectivity to AWS

### Business logic not working
- Check logs for initialization errors
- Verify environment variables are set correctly
- Ensure `.env` file is in the same directory as the executable (if using `.env` file)
