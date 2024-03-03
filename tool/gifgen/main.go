package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/docker/go-connections/nat"
	"github.com/google/subcommands"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/localstack"
)

const (
	vhsVersion        = "0.7.1"
	localstackVersion = "3.2.0"
)

func setupLocalstack() (string, func(), error) {
	ctx := context.Background()

	container, err := localstack.RunContainer(
		ctx,
		testcontainers.WithImage(fmt.Sprintf("localstack/localstack:%s", localstackVersion)),
		testcontainers.CustomizeRequest(
			testcontainers.GenericContainerRequest{
				ContainerRequest: testcontainers.ContainerRequest{
					Env: map[string]string{"SERVICES": "s3"},
				},
			},
		),
	)
	if err != nil {
		return "", nil, err
	}
	terminate := func() {
		if err := container.Terminate(ctx); err != nil {
			log.Fatalf("Failed to terminate container: %s", err)
		}
	}

	port, err := container.MappedPort(ctx, nat.Port("4566/tcp"))
	if err != nil {
		return "", nil, err
	}

	provider, err := testcontainers.NewDockerProvider()
	if err != nil {
		return "", nil, err
	}
	defer provider.Close()

	host, err := provider.DaemonHost(ctx)
	if err != nil {
		return "", nil, err
	}

	url := fmt.Sprintf("http://%s:%d", host, port.Int())
	return url, terminate, nil
}

func setupS3Client(url string) (*s3.Client, error) {
	customResolver := aws.EndpointResolverWithOptionsFunc(
		func(service, region string, opts ...interface{}) (aws.Endpoint, error) {
			return aws.Endpoint{URL: url, SigningRegion: region}, nil
		})
	cfg, err := config.LoadDefaultConfig(
		context.TODO(),
		config.WithRegion("us-east-1"),
		config.WithEndpointResolverWithOptions(customResolver),
		config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider("dummy", "dummy", "dummy"),
		),
	)
	if err != nil {
		return nil, err
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = true
	})
	return client, nil
}

func setupFixtures(s3Client *s3.Client) error {
	buckets := []string{
		"test-bucket-1",
		"test-bucket-2",
		"test-bucket-3",
	}
	objectKeys := []string{
		"text.txt",
		"foo/text1.txt",
		"foo/text2.txt",
		"foo/text3.txt",
		"foo/bar/text3.txt",
	}
	object := []byte("foo\nbar\nbaz\n")
	for _, bucket := range buckets {
		if err := createBucket(s3Client, bucket); err != nil {
			return err
		}
		for _, key := range objectKeys {
			uploadObject(s3Client, bucket, key, object)
		}
	}
	return nil
}

func createBucket(s3Client *s3.Client, name string) error {
	_, err := s3Client.CreateBucket(
		context.TODO(),
		&s3.CreateBucketInput{
			Bucket: aws.String(name),
		},
	)
	return err
}

func uploadObject(s3Client *s3.Client, bucket, key string, obj []byte) error {
	contentType := http.DetectContentType(obj)
	_, err := s3Client.PutObject(
		context.TODO(),
		&s3.PutObjectInput{
			Bucket:      aws.String(bucket),
			Key:         aws.String(key),
			Body:        bytes.NewReader(obj),
			ContentType: aws.String(contentType),
		},
	)
	return err
}

func checkVhs() error {
	var bufOut, bufErr bytes.Buffer
	cmd := exec.Command("vhs", "--version")
	cmd.Stdout = &bufOut
	cmd.Stderr = &bufErr
	versionStr := "vhs version v" + vhsVersion
	if err := cmd.Run(); err != nil || !strings.HasPrefix(bufOut.String(), versionStr) {
		return fmt.Errorf("vhs %s is not available. %v", vhsVersion, err)
	}
	return nil
}

func readTape(tapefile string, variables map[string]string) (string, error) {
	bytes, err := os.ReadFile(tapefile)
	if err != nil {
		return "", err
	}
	tape := string(bytes)
	for k, v := range variables {
		marker := fmt.Sprintf("${%v}", strings.ToUpper(k))
		tape = strings.ReplaceAll(tape, marker, v)
	}
	return tape, nil
}

func generateGif(tape string) error {
	cmd := exec.Command("vhs")
	cmd.Stdin = strings.NewReader(tape)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to generate gif. %v", err)
	}
	return nil
}

type generateCmd struct {
	tapefile string
	outpath  string
}

func (*generateCmd) Name() string { return "generate" }

func (*generateCmd) Synopsis() string { return "Generate gif" }

func (*generateCmd) Usage() string { return "generate -tape <file> -out <dir>\n" }

func (cmd *generateCmd) SetFlags(f *flag.FlagSet) {
	f.StringVar(&cmd.tapefile, "tape", "", "tape file path")
	f.StringVar(&cmd.outpath, "out", "", "output directory path")
}

func (cmd *generateCmd) Execute(_ context.Context, f *flag.FlagSet, args ...any) subcommands.ExitStatus {
	if err := cmd.run(); err != nil {
		log.Println(err)
		return subcommands.ExitFailure
	}
	return subcommands.ExitSuccess
}

func (cmd *generateCmd) run() error {
	if cmd.tapefile == "" {
		return errors.New("tape is not set")
	}
	if cmd.outpath == "" {
		return errors.New("out is not set")
	}
	if err := checkVhs(); err != nil {
		return err
	}

	url, terminate, err := setupLocalstack()
	if err != nil {
		return err
	}
	defer terminate()

	s3Client, err := setupS3Client(url)
	if err != nil {
		return err
	}

	if err := setupFixtures(s3Client); err != nil {
		return err
	}

	variables := map[string]string{
		"output_dir": cmd.outpath,
	}
	tape, err := readTape(cmd.tapefile, variables)
	if err != nil {
		return err
	}
	if err := generateGif(tape); err != nil {
		return err
	}

	return nil
}

func main() {
	subcommands.Register(subcommands.HelpCommand(), "")
	subcommands.Register(subcommands.CommandsCommand(), "")
	subcommands.Register(subcommands.FlagsCommand(), "")
	subcommands.Register(&generateCmd{}, "")
	flag.Parse()
	ctx := context.Background()
	os.Exit(int(subcommands.Execute(ctx)))
}
