package main

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/mattn/go-zglob"
)

type S3Upload struct {
	Config     *Config
	Conn       *s3.S3
	SourcePath string
}

func NewS3Upload(cfg *Config) (*S3Upload, error) {
	var err error
	s3c := &S3Upload{Config: cfg}
	s3c.Conn, err = s3c.newSession()
	if err != nil {
		return nil, err
	}
	s3c.SourcePath, err = filepath.Abs(cfg.S3.Source)
	if err != nil {
		return nil, err
	}
	return s3c, nil
}

func (s *S3Upload) newSession() (*s3.S3, error) {
	cfg := s.Config

	awsConfig := &aws.Config{}

	sess, err := session.NewSession(awsConfig)
	if err != nil {
		return nil, err
	}
	sess.Config.WithCredentials(credentials.NewStaticCredentials(cfg.S3.AccessKey, cfg.S3.SecretKey, ""))

	region := cfg.S3.Region
	if region == "" {
		region, err = s3manager.GetBucketRegion(context.Background(), sess, cfg.S3.Bucket, "us-west-2")
		if err != nil {
			return nil, err
		}
	}

	if region == "" {
		return nil, errors.New("unknown region")
	}
	sess.Config.WithRegion(region)

	return s3.New(sess), nil
}

func (s *S3Upload) isUploadableFile(path string) (bool, error) {
	for _, pat := range s.Config.S3.Ignore {
		match, err := zglob.Match(pat, path)
		if err != nil {
			return false, err
		}
		if match {
			return false, nil
		}
	}
	return true, nil
}

func (s *S3Upload) sourceFiles() ([]string, error) {
	var files []string
	source := s.SourcePath

	err := filepath.Walk(source, func(path string, info os.FileInfo, err error) error {
		cpath := strings.TrimPrefix(path, s.SourcePath)
		if cpath == "" {
			return nil
		}
		cpath = cpath[1:]

		// Skip ignored files
		ok, err := s.isUploadableFile(cpath)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}

		// Skip if path is directory
		fi, err := os.Stat(path)
		if err != nil {
			return err
		}
		if fi.IsDir() {
			return nil
		}

		// Add to the list of files to upload
		files = append(files, cpath)
		return nil
	})

	if err != nil {
		return nil, err
	}

	return files, nil
}

func (s *S3Upload) uploadFile(path string, dryrun bool) (int, error) {
	num := 0
	s3c := s.Conn

	file, err := os.Open(filepath.Join(s.SourcePath, path))
	if err != nil {
		return num, err
	}
	defer file.Close()

	mimeType := mime.TypeByExtension(filepath.Ext(path))
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	destPath := filepath.Join("/", s.Config.S3.Prefix, path)

	if dryrun {
		fmt.Printf("[DRYRUN] uploading %s ...\n", destPath)
	} else {
		fmt.Printf("uploading %s ...\n", destPath)
	}

	acl := s.Config.S3.ACL
	if acl == "" {
		acl = "private"
	}

	obj := &s3.PutObjectInput{
		Bucket:      aws.String(s.Config.S3.Bucket),
		Key:         aws.String(destPath),
		ACL:         aws.String(acl),
		ContentType: aws.String(mimeType),
		Body:        file,
	}

	if s.Config.S3.CacheControl != "" {
		obj.CacheControl = aws.String(s.Config.S3.CacheControl)
	}

	if !dryrun {
		req, _ := s3c.PutObjectRequest(obj)
		if err := req.Send(); err != nil {
			return num, err
		}
		num += 1
	}

	return num, nil
}

func (s *S3Upload) Upload(parallel int, dryrun bool) (uint64, error) {
	files, err := s.sourceFiles()
	if err != nil {
		return 0, err
	}

	fch := make(chan string, len(files))
	for _, path := range files {
		fch <- path
	}
	close(fch)

	var num uint64

	var wg sync.WaitGroup
	for i := 0; i < parallel; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for path := range fch {
				numRetries := 30
			RETRY:
				n, err := s.uploadFile(path, dryrun)
				if err != nil {
					_, ok := err.(awserr.Error)
					if ok {
						numRetries -= 1
						if numRetries > 0 {
							// retry in 1 second
							fmt.Printf("failed to upload %s, retrying in 1 second ...\n", path)
							time.Sleep(1 * time.Second)
							goto RETRY
						} else {
							panic(err)
						}
					} else {
						panic(fmt.Sprintf("unknown error! %v", err))
					}
				}
				atomic.AddUint64(&num, uint64(n))
			}
		}(i)
	}

	wg.Wait()
	return num, nil
}
