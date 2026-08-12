package main

import (
	"crypto/rand"
	"fmt"
	"io"
	"log"
	"strings"
)

const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func generateKey(reader io.Reader) (string, error) {
	const length = 48
	buf := make([]byte, length)
	if _, err := io.ReadFull(reader, buf); err != nil {
		return "", err
	}
	out := make([]byte, length)
	for i, b := range buf {
		out[i] = alphabet[int(b)%len(alphabet)]
	}
	return "sk-" + string(out), nil
}

func main() {
	key, err := generateKey(rand.Reader)
	if err != nil {
		log.Fatal(err)
	}
	if !strings.HasPrefix(key, "sk-") {
		log.Fatalf("generated key %q is missing sk- prefix", key)
	}
	fmt.Println(key)
}
