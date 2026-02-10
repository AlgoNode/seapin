package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"

	cid "github.com/ipfs/go-cid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	mh "github.com/multiformats/go-multihash"
)

func main() {
	endpoint := envOr("S3_ENDPOINT", "minio:9000")
	bucket := envOr("S3_BUCKET", "ipfs")
	accessKey := envOr("S3_ACCESS_KEY", "minioadmin")
	secretKey := envOr("S3_SECRET_KEY", "minioadmin")
	useSSL, _ := strconv.ParseBool(envOr("S3_USE_SSL", "false"))
	listenAddr := envOr("LISTEN_ADDR", ":8080")

	s3, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: useSSL,
	})
	if err != nil {
		log.Fatalf("failed to create s3 client: %v", err)
	}

	ensureBucket(s3, bucket)

	mux := http.NewServeMux()

	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "seapin ipfs gateway")
	})

	mux.HandleFunc("GET /ipfs/{cid}", handleIPFS(s3, bucket))
	mux.HandleFunc("POST /upload", handleUpload(s3, bucket))

	log.Printf("listening on %s", listenAddr)
	log.Fatal(http.ListenAndServe(listenAddr, mux))
}

func handleIPFS(s3 *minio.Client, bucket string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cidStr := r.PathValue("cid")

		// Validate CID
		_, err := cid.Decode(cidStr)
		if err != nil {
			http.Error(w, "invalid CID", http.StatusBadRequest)
			return
		}

		obj, err := s3.GetObject(context.Background(), bucket, cidStr, minio.GetObjectOptions{})
		if err != nil {
			log.Printf("s3 GetObject error: %v", err)
			http.Error(w, "storage error", http.StatusBadGateway)
			return
		}
		defer obj.Close()

		info, err := obj.Stat()
		if err != nil {
			resp := minio.ToErrorResponse(err)
			if resp.Code == "NoSuchKey" {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			log.Printf("s3 Stat error: %v", err)
			http.Error(w, "storage error", http.StatusBadGateway)
			return
		}

		contentType := info.ContentType
		if contentType == "" {
			contentType = "application/octet-stream"
		}

		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
		w.Header().Set("Cache-Control", "public, max-age=29030400, immutable")
		w.Header().Set("X-Ipfs-Path", "/ipfs/"+cidStr)
		w.WriteHeader(http.StatusOK)

		io.Copy(w, obj)
	}
}

func handleUpload(s3 *minio.Client, bucket string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		file, header, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "missing file field", http.StatusBadRequest)
			return
		}
		defer file.Close()

		data, err := io.ReadAll(file)
		if err != nil {
			http.Error(w, "failed to read file", http.StatusInternalServerError)
			return
		}

		c, err := computeCID(data)
		if err != nil {
			http.Error(w, "failed to compute CID", http.StatusInternalServerError)
			return
		}
		cidStr := c.String()

		contentType := header.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "application/octet-stream"
		}

		_, err = s3.PutObject(context.Background(), bucket, cidStr, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{
			ContentType: contentType,
		})
		if err != nil {
			log.Printf("s3 PutObject error: %v", err)
			http.Error(w, "storage error", http.StatusBadGateway)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{
			"cid": cidStr,
			"url": "/ipfs/" + cidStr,
		})
	}
}

// computeCID produces a CIDv1 with raw codec and SHA2-256, matching
// ipfs add --cid-version=1 --raw-leaves for single-chunk files.
func computeCID(data []byte) (cid.Cid, error) {
	hash := sha256.Sum256(data)
	multihash, err := mh.Encode(hash[:], mh.SHA2_256)
	if err != nil {
		return cid.Undef, err
	}
	return cid.NewCidV1(cid.Raw, multihash), nil
}

func ensureBucket(s3 *minio.Client, bucket string) {
	ctx := context.Background()
	exists, err := s3.BucketExists(ctx, bucket)
	if err != nil {
		log.Fatalf("failed to check bucket %q: %v", bucket, err)
	}
	if exists {
		return
	}
	if err := s3.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
		log.Fatalf("failed to create bucket %q: %v", bucket, err)
	}
	log.Printf("created bucket %q", bucket)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
