# Use the official Golang image to create a build artifact.
FROM golang:1.21.1 AS builder

# Set the working directory inside the container.
WORKDIR /app

# Copy go mod and sum files
#COPY go.mod go.sum ./
COPY go.mod ./

# Download all dependencies.
RUN go mod download

# Copy the source code into the container.
COPY . .

# Build the Go app
RUN CGO_ENABLED=0 GOOS=linux go build -o main .

# Use a lightweight image for the final image.
FROM alpine:3

# Copy the binary.
COPY --from=builder /app/main /app/main

# Run the binary.
CMD ["/app/main"]
