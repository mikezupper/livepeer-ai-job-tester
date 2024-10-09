# Use the official Golang image as the base image
FROM golang:1.22 AS builder

# Set the working directory inside the container
WORKDIR /app

# Copy go.mod and go.sum files for dependency management
COPY go.mod ./

# Download dependencies
RUN go mod download

# Copy the entire project to the working directory
COPY . .

# Build the Go app and output it to the 'app' binary
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o jobtester ./cmd/ai-job-tester.go

# Use a minimal base image to run the application (Alpine Linux)
FROM alpine:latest
RUN apk add busybox-extras

ENV PATH=$PATH:/app/
# Set working directory
WORKDIR /app
# Copy the compiled binary from the builder stage
COPY --from=builder /app/jobtester ./
COPY --from=builder /app/entrypoint.sh ./
COPY --from=builder /app/test-assets/ ./test-assets/

# Ensure entrypoint.sh is executable
RUN chmod +x /app/entrypoint.sh
RUN chmod +x /app/jobtester

EXPOSE 7934

# Run the binary
ENTRYPOINT ["entrypoint.sh"]