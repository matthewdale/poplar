package rpc

import (
	"context"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/evergreen-ci/juniper/gopb"
	"github.com/evergreen-ci/pail"
	"github.com/evergreen-ci/poplar"
	"github.com/evergreen-ci/poplar/rpc/internal"
	"github.com/evergreen-ci/utility"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"
	"gopkg.in/h2non/gock.v1"
)

type mockClient struct {
	resultData []*gopb.ResultData
	endData    map[string]*gopb.MetricsSeriesEnd
}

func NewMockClient() *mockClient {
	return &mockClient{endData: map[string]*gopb.MetricsSeriesEnd{}}
}

func (mc *mockClient) CreateMetricSeries(_ context.Context, in *gopb.ResultData, _ ...grpc.CallOption) (*gopb.MetricsResponse, error) {
	mc.resultData = append(mc.resultData, in)
	return &gopb.MetricsResponse{Id: in.Id.TestName, Success: true}, nil
}
func (*mockClient) AttachResultData(_ context.Context, _ *gopb.ResultData, _ ...grpc.CallOption) (*gopb.MetricsResponse, error) {
	return nil, nil
}
func (*mockClient) AttachArtifacts(_ context.Context, _ *gopb.ArtifactData, _ ...grpc.CallOption) (*gopb.MetricsResponse, error) {
	return nil, nil
}
func (*mockClient) AttachRollups(_ context.Context, _ *gopb.RollupData, _ ...grpc.CallOption) (*gopb.MetricsResponse, error) {
	return nil, nil
}
func (*mockClient) SendMetrics(_ context.Context, _ ...grpc.CallOption) (gopb.CedarPerformanceMetrics_SendMetricsClient, error) {
	return nil, nil
}
func (mc *mockClient) CloseMetrics(_ context.Context, in *gopb.MetricsSeriesEnd, _ ...grpc.CallOption) (*gopb.MetricsResponse, error) {
	mc.endData[in.Id] = in
	return &gopb.MetricsResponse{Success: true}, nil
}

func mockUploadReport(ctx context.Context, report *poplar.Report, client gopb.CedarPerformanceMetricsClient, serialize bool, AWSAccessKey string, AWSSecretKey string, AWSToken string, dryRun bool) error {
	opts := UploadReportOptions{
		Report:          report,
		SerializeUpload: serialize,
		AWSAccessKey:    AWSAccessKey,
		AWSSecretKey:    AWSSecretKey,
		AWSToken:        AWSToken,
		DryRun:          dryRun,
	}
	if err := opts.convertAndUploadArtifacts(ctx); err != nil {
		return errors.Wrap(err, "converting and uploading artifacts for report")
	}
	return errors.Wrap(uploadTests(ctx, client, report, report.Tests, dryRun), "uploading tests for report")
}

func TestClient(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	testdataDir := filepath.Join("..", "testdata")
	s3Name := "build-test-curator"
	s3Prefix := "poplar-client-test"
	s3Opts := pail.S3Options{
		Name:   s3Name,
		Prefix: s3Prefix,
		Region: "us-east-1",
	}
	AWSAccessKey := "fake-access-key"
	AWSSecretKey := "fake-secret-key"
	AWSToken := "fake-aws-token"

	client := utility.GetHTTPClient()
	defer utility.PutHTTPClient(client)

	s3Bucket, err := pail.NewS3BucketWithHTTPClient(client, s3Opts)
	require.NoError(t, err)

	report := generateTestReport(testdataDir, s3Name, s3Prefix, false)
	expectedTests := []poplar.Test{
		report.Tests[0],
		report.Tests[0].SubTests[0],
		report.Tests[1],
		report.Tests[1].SubTests[0],
	}
	expectedParents := map[string]string{
		"test0":  "",
		"test00": "test0",
		"test1":  "",
		"test10": "test1",
	}
	for i := range expectedTests {
		for j := range expectedTests[i].Artifacts {
			require.NoError(t, expectedTests[i].Artifacts[j].Convert(ctx))
			require.NoError(t, expectedTests[i].Artifacts[j].SetBucketInfo(report.BucketConf))
			require.NoError(t, os.RemoveAll(filepath.Join(testdataDir, expectedTests[i].Artifacts[j].Path)))
		}
	}

	defer func() {
		for _, test := range expectedTests {
			for _, artifact := range test.Artifacts {
				assert.NoError(t, s3Bucket.Remove(ctx, artifact.Path))
				assert.NoError(t, os.RemoveAll(filepath.Join(testdataDir, artifact.Path)))
			}
		}
	}()
	t.Run("WetRunUploadToDataPipes", func(t *testing.T) {
		testReport := generateTestReport(testdataDir, s3Name, s3Prefix, false)
		client := utility.GetHTTPClient()
		defer utility.PutHTTPClient(client)
		opts := UploadReportOptions{
			Report:              &testReport,
			SerializeUpload:     false,
			AWSAccessKey:        AWSAccessKey,
			AWSSecretKey:        AWSSecretKey,
			AWSToken:            AWSToken,
			DataPipesHost:       "https://fakeurl.mock",
			DataPipesRegion:     "fake-region",
			DataPipesHTTPClient: client,
			DryRun:              false,
		}
		require.Error(t, uploadResultsToDataPipes(&opts))
		defer gock.Off()
		defer gock.RestoreClient(client)
		gock.InterceptClient(client)

		gock.New("https://fakeurl.mock").
			Put("/results/evergreen/task/taskID/execution/2/type/cedar-report/name/*").
			Reply(200).
			JSON(map[string]interface{}{"url": "https://s3-bucket-location.mock/signed_string", "expiration_secs": 1800})

		gock.New("https://s3-bucket-location.mock").
			Put("/signed_string").
			Reply(200).
			JSON(map[string]interface{}{})
		require.NoError(t, uploadResultsToDataPipes(&opts))
	})
	t.Run("DryRunUploadtoDataPipes", func(t *testing.T) {
		testReport := generateTestReport(testdataDir, s3Name, s3Prefix, false)
		opts := UploadReportOptions{
			Report:          &testReport,
			SerializeUpload: false,
			AWSAccessKey:    AWSAccessKey,
			AWSSecretKey:    AWSSecretKey,
			AWSToken:        AWSToken,
			DryRun:          true,
		}
		require.NoError(t, uploadResultsToDataPipes(&opts))
	})
	t.Run("WetRun", func(t *testing.T) {
		for _, serialize := range []bool{true, false} {
			testReport := generateTestReport(testdataDir, s3Name, s3Prefix, false)
			mc := NewMockClient()
			require.NoError(t, mockUploadReport(ctx, &testReport, mc, serialize, AWSAccessKey, AWSSecretKey, AWSToken, false))
			require.Len(t, mc.resultData, len(expectedTests))
			require.Equal(t, len(mc.resultData), len(mc.endData))
			for i, result := range mc.resultData {
				assert.Equal(t, testReport.Project, result.Id.Project)
				assert.Equal(t, testReport.Version, result.Id.Version)
				assert.Equal(t, testReport.Order, int(result.Id.Order))
				assert.Equal(t, testReport.Variant, result.Id.Variant)
				assert.Equal(t, testReport.TaskName, result.Id.TaskName)
				assert.Equal(t, testReport.TaskID, result.Id.TaskId)
				assert.Equal(t, testReport.Mainline, result.Id.Mainline)
				assert.Equal(t, testReport.Execution, int(result.Id.Execution))
				assert.Equal(t, expectedTests[i].Info.TestName, result.Id.TestName)
				assert.Equal(t, expectedTests[i].Info.Trial, int(result.Id.Trial))
				assert.Equal(t, expectedTests[i].Info.Tags, result.Id.Tags)
				assert.Equal(t, expectedTests[i].Info.Arguments, result.Id.Arguments)
				assert.Equal(t, expectedParents[expectedTests[i].Info.TestName], result.Id.Parent)
				var expectedCreatedAt *timestamppb.Timestamp
				var expectedCompletedAt *timestamppb.Timestamp
				if !expectedTests[i].CreatedAt.IsZero() {
					expectedCreatedAt = timestamppb.New(expectedTests[i].CreatedAt)
				}
				if !expectedTests[i].CompletedAt.IsZero() {
					expectedCompletedAt = timestamppb.New(expectedTests[i].CompletedAt)
				}
				assert.Equal(t, expectedCreatedAt, result.Id.CreatedAt)
				assert.Equal(t, expectedCompletedAt, mc.endData[result.Id.TestName].CompletedAt)

				require.Len(t, result.Artifacts, len(expectedTests[i].Artifacts))
				for j, artifact := range expectedTests[i].Artifacts {
					require.NoError(t, artifact.Validate())
					expectedArtifact := internal.ExportArtifactInfo(&artifact)
					expectedArtifact.Location = gopb.StorageLocation_CEDAR_S3
					assert.Equal(t, expectedArtifact, result.Artifacts[j])
					r, err := s3Bucket.Get(ctx, artifact.Path)
					require.NoError(t, err)
					remoteData, err := ioutil.ReadAll(r)
					require.NoError(t, err)
					f, err := os.Open(filepath.Join(testdataDir, artifact.Path))
					require.NoError(t, err)
					localData, err := ioutil.ReadAll(f)
					require.NoError(t, err)
					assert.Equal(t, localData, remoteData)
					require.NoError(t, f.Close())
				}

				require.Len(t, result.Rollups, len(expectedTests[i].Metrics))
				for k, metric := range expectedTests[i].Metrics {
					assert.Equal(t, internal.ExportRollup(&metric), result.Rollups[k])
				}
			}
		}
	})

	for _, test := range expectedTests {
		for _, artifact := range test.Artifacts {
			require.NoError(t, s3Bucket.Remove(ctx, artifact.Path))
			require.NoError(t, os.RemoveAll(filepath.Join(testdataDir, artifact.Path)))
		}
	}

	t.Run("DryRun", func(t *testing.T) {
		for _, serialize := range []bool{true, false} {
			testReport := generateTestReport(testdataDir, s3Name, s3Prefix, false)
			mc := NewMockClient()
			require.NoError(t, mockUploadReport(ctx, &testReport, mc, serialize, AWSAccessKey, AWSSecretKey, AWSToken, true))
			assert.Empty(t, mc.resultData)
			assert.Empty(t, mc.endData)
			for _, expectedTest := range expectedTests {
				for _, artifact := range expectedTest.Artifacts {
					require.NoError(t, artifact.Validate())
					r, err := s3Bucket.Get(ctx, artifact.Path)
					assert.Error(t, err)
					assert.Nil(t, r)
					_, err = os.Stat(filepath.Join(testdataDir, artifact.Path))
					require.NoError(t, err)
				}
			}
		}
	})

	for _, test := range expectedTests {
		for _, artifact := range test.Artifacts {
			require.NoError(t, s3Bucket.Remove(ctx, artifact.Path))
			require.NoError(t, os.RemoveAll(filepath.Join(testdataDir, artifact.Path)))
		}
	}

	t.Run("DuplicateMetricName", func(t *testing.T) {
		for _, serialize := range []bool{true, false} {
			testReport := generateTestReport(testdataDir, s3Name, s3Prefix, true)
			mc := NewMockClient()
			assert.Error(t, mockUploadReport(ctx, &testReport, mc, serialize, AWSAccessKey, AWSSecretKey, AWSToken, true))
			assert.Empty(t, mc.resultData)
			assert.Empty(t, mc.endData)
		}
	})
}

func generateTestReport(testdataDir, s3Name, s3Prefix string, duplicateMetric bool) poplar.Report {
	report := poplar.Report{
		Project:   "project",
		Version:   "version",
		Order:     2,
		Variant:   "variant",
		TaskName:  "taskName",
		TaskID:    "taskID",
		Mainline:  true,
		Execution: 2,
		Requester: "RepotrackerVersionRequester",

		BucketConf: poplar.BucketConfiguration{
			Name:   s3Name,
			Region: "us-east-1",
		},

		Tests: []poplar.Test{
			{
				Info: poplar.TestInfo{
					TestName:  "test0",
					Trial:     2,
					Tags:      []string{"tag0", "tag1"},
					Arguments: map[string]int32{"thread_level": 1},
				},
				Artifacts: []poplar.TestArtifact{
					{
						Bucket:           s3Name,
						Prefix:           s3Prefix,
						Path:             "bson_example.ftdc",
						LocalFile:        filepath.Join(testdataDir, "bson_example.bson"),
						ConvertBSON2FTDC: true,
					},
					{
						Prefix:      s3Prefix,
						LocalFile:   filepath.Join(testdataDir, "bson_example.bson"),
						ConvertGzip: true,
					},
				},
				CreatedAt:   time.Date(2018, time.July, 4, 12, 0, 0, 0, time.UTC),
				CompletedAt: time.Date(2018, time.July, 4, 12, 1, 0, 0, time.UTC),
				SubTests: []poplar.Test{
					{
						Info: poplar.TestInfo{
							TestName: "test00",
						},
						Metrics: []poplar.TestMetrics{
							{
								Name:    "mean",
								Version: 1,
								Value:   1.5,
								Type:    "MEAN",
							},
							{
								Name:    "sum",
								Version: 1,
								Value:   10,
								Type:    "SUM",
							},
						},
					},
				},
			},
			{
				Info: poplar.TestInfo{
					TestName: "test1",
				},
				Artifacts: []poplar.TestArtifact{
					{
						Bucket:           s3Name,
						Prefix:           s3Prefix,
						Path:             "json_example.ftdc",
						LocalFile:        filepath.Join(testdataDir, "json_example.json"),
						CreatedAt:        time.Date(2018, time.July, 4, 11, 59, 0, 0, time.UTC),
						ConvertJSON2FTDC: true,
					},
				},
				SubTests: []poplar.Test{
					{
						Info: poplar.TestInfo{
							TestName: "test10",
						},
						Metrics: []poplar.TestMetrics{
							{
								Name:    "mean",
								Version: 1,
								Value:   1.5,
								Type:    "MEAN",
							},
							{
								Name:    "sum",
								Version: 1,
								Value:   10,
								Type:    "SUM",
							},
						},
					},
				},
			},
		},
	}

	if duplicateMetric {
		report.Tests[0].SubTests[0].Metrics = append(
			report.Tests[0].SubTests[0].Metrics,
			poplar.TestMetrics{
				Name:    "mean",
				Version: 1,
				Value:   2,
				Type:    "MEAN",
			},
		)
	}

	return report
}
