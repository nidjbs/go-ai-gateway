package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"flag"
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
	printHash := flag.Bool("sha256", false, "print the sha256:<hex> digest for config files instead of the plaintext key")
	flag.Parse()
	key, err := generateKey(rand.Reader)
	if err != nil {
		log.Fatal(err)
	}
	if !strings.HasPrefix(key, "sk-") {
		log.Fatalf("generated key %q is missing sk- prefix", key)
	}
	if *printHash {
		sum := sha256.Sum256([]byte(key))
		fmt.Println("sha256:" + hex.EncodeToString(sum[:]))
		return
	}
	fmt.Println(key)
}
