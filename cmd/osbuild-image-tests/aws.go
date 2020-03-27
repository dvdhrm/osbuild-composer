package main

import (
	"encoding/base64"
	"errors"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/google/uuid"
	"github.com/osbuild/osbuild-composer/internal/common"
	"github.com/osbuild/osbuild-composer/internal/upload/awsupload"
	"log"
	"os"
)

type awsCredentials struct {
	AccessKeyId     string
	SecretAccessKey string
	Region          string
	Bucket          string
}

func getAWSCredentialsFromEnv() (*awsCredentials, error) {
	accessKeyId, akExists := os.LookupEnv("AWS_ACCESS_KEY_ID")
	secretAccessKey, sakExists := os.LookupEnv("AWS_SECRET_ACCESS_KEY")
	region, regionExists := os.LookupEnv("AWS_REGION")
	bucket, bucketExists := os.LookupEnv("AWS_BUCKET")

	// Workaround Travis security feature. If non of the variables is set, just ignore the test
	if !akExists && !sakExists && !bucketExists && !regionExists {
		return nil, nil
	}
	// If only one/two of them are not set, then fail
	if !akExists || !sakExists || !bucketExists || !regionExists {
		return nil, errors.New("not all required env variables were set")
	}

	return &awsCredentials{
		AccessKeyId:     accessKeyId,
		SecretAccessKey: secretAccessKey,
		Region:          region,
		Bucket:          bucket,
	}, nil
}

func generateRandomName(prefix string) (string, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}

	return prefix + id.String(), nil
}

var userData = `#cloud-config
user: redhat
ssh_authorized_keys:
  - ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQC61wMCjOSHwbVb4VfVyl5sn497qW4PsdQ7Ty7aD6wDNZ/QjjULkDV/yW5WjDlDQ7UqFH0Sr7vywjqDizUAqK7zM5FsUKsUXWHWwg/ehKg8j9xKcMv11AkFoUoujtfAujnKODkk58XSA9whPr7qcw3vPrmog680pnMSzf9LC7J6kXfs6lkoKfBh9VnlxusCrw2yg0qI1fHAZBLPx7mW6+me71QZsS6sVz8v8KXyrXsKTdnF50FjzHcK9HXDBtSJS5wA3fkcRYymJe0o6WMWNdgSRVpoSiWaHHmFgdMUJaYoCfhXzyl7LtNb3Q+Sveg+tJK7JaRXBLMUllOlJ6ll5Hod
`

func encodeBase64(input string) string {
	return base64.StdEncoding.EncodeToString([]byte(input))
}

func withBootedImageInEC2(image string, c *awsCredentials, f func(address string) error) error {
	creds := credentials.NewStaticCredentials(c.AccessKeyId, c.SecretAccessKey, "")
	sess, err := session.NewSession(&aws.Config{
		Credentials: creds,
		Region:      aws.String(c.Region),
	})
	common.PanicOnError(err)

	s := s3.New(sess)
	e := ec2.New(sess)

	uploader, err := awsupload.New(c.Region, c.AccessKeyId, c.SecretAccessKey)
	common.PanicOnError(err)

	s3Key, err := generateRandomName("osbuild-image-tests-s3-")
	common.PanicOnError(err)

	_, err = uploader.Upload(image, c.Bucket, s3Key)
	common.PanicOnError(err)

	defer func() {
		_, err := s.DeleteObject(&s3.DeleteObjectInput{
			Bucket: aws.String(c.Bucket),
			Key:    aws.String(s3Key),
		})
		if err != nil {
			log.Print(err)
		}

	}()

	imageName, err := generateRandomName("osbuild-image-tests-image-")
	common.PanicOnError(err)

	_, err = uploader.Register(imageName, c.Bucket, s3Key)
	common.PanicOnError(err)

	imageDescriptions, err := e.DescribeImages(&ec2.DescribeImagesInput{
		Filters: []*ec2.Filter{
			{
				Name: aws.String("name"),
				Values: []*string{
					aws.String(imageName),
				},
			},
		},
	})
	common.PanicOnError(err)

	imageId := imageDescriptions.Images[0].ImageId
	snapshotId := imageDescriptions.Images[0].BlockDeviceMappings[0].Ebs.SnapshotId

	defer func() {
		_, err := e.DeleteSnapshot(&ec2.DeleteSnapshotInput{
			SnapshotId: snapshotId,
		})

		if err != nil {
			log.Print(err)
		}
	}()

	defer func() {
		_, err := e.DeregisterImage(&ec2.DeregisterImageInput{
			ImageId: imageId,
		})

		if err != nil {
			log.Print(err)
		}
	}()

	randomUUID, err := uuid.NewRandom()
	common.PanicOnError(err)

	groupName := "image-tests-" + randomUUID.String()

	securityGroup, err := e.CreateSecurityGroup(&ec2.CreateSecurityGroupInput{
		GroupName:   aws.String(groupName),
		Description: aws.String("as"),
	})
	common.PanicOnError(err)

	defer func() {
		_, err = e.DeleteSecurityGroup(&ec2.DeleteSecurityGroupInput{
			GroupId: securityGroup.GroupId,
		})

		if err != nil {
			log.Print(err)
		}
	}()

	_, err = e.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
		CidrIp:     aws.String("0.0.0.0/0"),
		GroupId:    securityGroup.GroupId,
		FromPort:   aws.Int64(22),
		ToPort:     aws.Int64(22),
		IpProtocol: aws.String("tcp"),
	})
	common.PanicOnError(err)

	res, err := e.RunInstances(&ec2.RunInstancesInput{
		MaxCount:         aws.Int64(1),
		MinCount:         aws.Int64(1),
		ImageId:          imageId,
		InstanceType:     aws.String("t3.micro"),
		SecurityGroupIds: []*string{securityGroup.GroupId},
		UserData:         aws.String(encodeBase64(userData)),
	})
	common.PanicOnError(err)

	describeInstanceInput := &ec2.DescribeInstancesInput{
		InstanceIds: []*string{
			res.Instances[0].InstanceId,
		},
	}

	defer func() {
		_, err = e.TerminateInstances(&ec2.TerminateInstancesInput{
			InstanceIds: []*string{
				res.Instances[0].InstanceId,
			},
		})
		if err != nil {
			log.Print(err)
			return
		}

		err = e.WaitUntilInstanceTerminated(describeInstanceInput)
		if err != nil {
			log.Print(err)
		}
	}()

	err = e.WaitUntilInstanceRunning(describeInstanceInput)
	common.PanicOnError(err)

	out, err := e.DescribeInstances(describeInstanceInput)
	common.PanicOnError(err)

	return f(*out.Reservations[0].Instances[0].PublicIpAddress)
}
