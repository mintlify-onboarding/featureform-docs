//go:build k8s
// +build k8s

package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/featureform/config"

	"github.com/featureform/helpers"
	"github.com/featureform/metadata"
	pc "github.com/featureform/provider/provider_config"
	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/mitchellh/mapstructure"

	filestore "github.com/featureform/filestore"
	"github.com/parquet-go/parquet-go"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
)

func uuidWithoutDashes() string {
	return fmt.Sprintf("a%s", strings.ReplaceAll(uuid.New().String(), "-", ""))
}

// Tests that both Legacy and new ExecutorConfig can be properly deserialized
func TestDeserializeExecutorConfig(t *testing.T) {
	expectedConfig := pc.K8sConfig{
		ExecutorType: pc.K8s,
		ExecutorConfig: pc.ExecutorConfig{
			DockerImage: "",
		},
		StoreType: filestore.Azure,
		StoreConfig: &pc.AzureFileStoreConfig{
			AccountName:   "account name",
			AccountKey:    "account key",
			ContainerName: "container name",
			Path:          "container path",
		},
	}

	testConfig := map[string]interface{}{
		"ExecutorType":   expectedConfig.ExecutorType,
		"ExecutorConfig": "",
		"StoreType":      expectedConfig.StoreType,
		"StoreConfig": &pc.AzureFileStoreConfig{
			AccountName:   expectedConfig.StoreConfig.(*pc.AzureFileStoreConfig).AccountName,
			AccountKey:    expectedConfig.StoreConfig.(*pc.AzureFileStoreConfig).AccountKey,
			ContainerName: expectedConfig.StoreConfig.(*pc.AzureFileStoreConfig).ContainerName,
			Path:          expectedConfig.StoreConfig.(*pc.AzureFileStoreConfig).Path,
		},
	}

	type testCase struct {
		GivenExecutorConfig    interface{}
		ExpectedExecutorConfig pc.ExecutorConfig
	}

	testCases := map[string]testCase{
		"Legacy Config": testCase{
			"",
			pc.ExecutorConfig{
				DockerImage: "",
			},
		},
		"Empty Image": testCase{
			map[string]interface{}{
				"docker_image": "",
			},
			pc.ExecutorConfig{
				DockerImage: "",
			},
		},
		"Named Image": testCase{
			map[string]interface{}{
				"docker_image": "repo/image:tag",
			},
			pc.ExecutorConfig{
				DockerImage: "repo/image:tag",
			},
		},
	}

	for name, c := range testCases {
		t.Run(name, func(t *testing.T) {
			testConfig["ExecutorConfig"] = c.GivenExecutorConfig
			expectedConfig.ExecutorConfig = c.ExpectedExecutorConfig
			serializedConfig, err := json.Marshal(testConfig)
			if err != nil {
				t.Errorf("Could not serialize config %s", err.Error())
			}
			receivedConfig := pc.K8sConfig{}
			receivedConfig.Deserialize(serializedConfig)

			if !(reflect.DeepEqual(expectedConfig, receivedConfig)) {
				t.Errorf("\nExpected %#v\nGot %#v\n", expectedConfig, receivedConfig)
			}
		})
	}
}

func TestBlobInterfaces(t *testing.T) {
	fileStoreTests := map[string]func(*testing.T, FileStore){
		"Test Filestore Read and Write": testFilestoreReadAndWrite,
		"Test Exists":                   testExists,
		"Test Not Exists":               testNotExists,
		"Test Serve":                    testServe,
		"Test Serve Directory":          testServeDirectory,
		"Test Delete":                   testDelete,
		"Test Delete All":               testDeleteAll,
		"Test Newest file":              testNewestFile,
		"Test Num Rows":                 testNumRows,
		"Test File Upload and Download": testFileUploadAndDownload,
	}

	err := godotenv.Load("../.env")

	mydir, err := os.Getwd()
	if err != nil {
		t.Fatalf("could not get working directory")
	}

	directoryPath := fmt.Sprintf("%s/scripts/k8s/tests/test_files/output/go_tests", mydir)
	_ = os.MkdirAll(directoryPath, os.ModePerm)

	fileStoreConfig := pc.LocalFileStoreConfig{DirPath: fmt.Sprintf(`file:///%s`, directoryPath)}
	serializedFileConfig, err := fileStoreConfig.Serialize()
	if err != nil {
		t.Fatalf("failed to serialize file store config: %v", err)
	}
	fileFileStore, err := NewLocalFileStore(serializedFileConfig)
	if err != nil {
		t.Fatalf("failed to create new file blob store: %v", err)
	}

	azureStoreConfig := &pc.AzureFileStoreConfig{
		AccountName:   helpers.GetEnv("AZURE_ACCOUNT_NAME", ""),
		AccountKey:    helpers.GetEnv("AZURE_ACCOUNT_KEY", ""),
		ContainerName: helpers.GetEnv("AZURE_CONTAINER_NAME", ""),
		// Path:          "testdirectory/testpath",
		Path: "refactor_filepath_tests",
	}
	serializedAzureConfig, err := azureStoreConfig.Serialize()
	if err != nil {
		t.Fatalf("failed to serialize azure store config: %v", err)
	}
	azureFileStore, err := NewAzureFileStore(serializedAzureConfig)
	if err != nil {
		t.Fatalf("failed to create new azure blob store: %v", err)
	}

	hdfsConfig := pc.HDFSFileStoreConfig{
		Host:     "localhost",
		Port:     "9000",
		Username: "hduser",
	}

	serializedHDFSConfig, err := hdfsConfig.Serialize()
	if err != nil {
		t.Fatalf("failed to create serialize hdfs blob store: %v", err)
	}

	hdfsFileStore, err := NewHDFSFileStore(serializedHDFSConfig)
	if err != nil {
		t.Fatalf("failed to create new hdfs blob store: %v", err)
	}

	blobProviders := map[string]FileStore{
		"File":  fileFileStore,
		"Azure": azureFileStore,
		"HDFS":  hdfsFileStore,
	}
	for testName, fileTest := range fileStoreTests {
		fileTest = fileTest
		testName = testName
		for blobName, blobProvider := range blobProviders {
			if blobName != "HDFS" {
				continue
			}
			blobName = blobName
			blobProvider = blobProvider
			t.Run(fmt.Sprintf("%s: %s", testName, blobName), func(t *testing.T) {
				fileTest(t, blobProvider)
			})
		}
	}
	for _, blobProvider := range blobProviders {
		blobProvider.Close()
	}
}

func testFileUploadAndDownload(t *testing.T, store FileStore) {
	// Need to get the working directory because the LocalFilepath will use it to create the full path
	// Currently, the LocalFilePath will only work with absolute paths
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("could not get working directory")
	}

	testId := uuidWithoutDashes()
	fileContent := "testing file upload"
	sourceFile := fmt.Sprintf("%s/fileUploadTest_%s.txt", wd, testId)
	destFile := fmt.Sprintf("fileUploadTest_%s.txt", testId)
	localDestFile := fmt.Sprintf("%s/fileDownloadTest_%s.txt", wd, testId)

	f, err := os.Create(sourceFile)
	if err != nil {
		t.Fatalf("could not create file to upload because %v", err)
	}
	defer f.Close()

	f.Write([]byte(fileContent))

	sourcePath := filestore.LocalFilepath{}
	if err := sourcePath.SetKey(sourceFile); err != nil {
		t.Fatalf("could not set source path because %v", err)
	}

	destinationPath, err := store.CreateFilePath(destFile)
	if err != nil {
		t.Fatalf("could not create destination file path because %v", err)
	}

	err = store.Upload(&sourcePath, destinationPath)
	if err != nil {
		t.Fatalf("could not upload file because %v", err)
	}

	exists, err := store.Exists(destinationPath)
	if err != nil {
		t.Fatalf("could not determine if file exists because %v", err)
	}
	if !exists {
		t.Fatalf("could not upload file to %s", destinationPath.ToURI())
	}

	localDestinationPath := filestore.LocalFilepath{}
	if err := localDestinationPath.SetKey(localDestFile); err != nil {
		t.Fatalf("could not set local destination path because %v", err)
	}

	err = store.Download(destinationPath, &localDestinationPath)
	if err != nil {
		t.Fatalf("could not download %s file to %s because %v", destinationPath.ToURI(), localDestinationPath.ToURI(), err)
	}

	content, err := ioutil.ReadFile(localDestinationPath.ToURI())
	if err != nil {
		t.Fatalf("could not read local file at %s because %v", localDestinationPath.ToURI(), err)
	}

	if string(content) != fileContent {
		t.Fatalf("the file contents are not the same. Got %s but expected %s", string(content), fileContent)
	}
}

func testFilestoreReadAndWrite(t *testing.T, store FileStore) {
	testWrite := []byte("example data")
	testKey := uuidWithoutDashes()
	testFilePath, err := store.CreateFilePath(testKey)
	if err != nil {
		t.Fatalf("Could not create test filepath: %v", err)
	}
	exists, err := store.Exists(testFilePath)
	if exists {
		t.Fatalf("Exists when not yet written")
	}
	if err := store.Write(testFilePath, testWrite); err != nil {
		t.Fatalf("Failure writing data %s to key %s: %v", string(testWrite), testFilePath.ToURI(), err)
	}
	exists, err = store.Exists(testFilePath)
	if err != nil {
		t.Fatalf("Failure checking existence of key %s: %v", testKey, err)
	}
	if !exists {
		t.Fatalf("Test key %s does not exist: %v", testKey, err)
	}
	readData, err := store.Read(testFilePath)
	if err != nil {
		t.Fatalf("Could not read key %s from store: %v", testFilePath.ToURI(), err)
	}
	if string(readData) != string(testWrite) {
		t.Fatalf("Read data does not match written data: %s != %s", readData, testWrite)
	}
	if err := store.Delete(testFilePath); err != nil {
		t.Fatalf("Failed to delete test file with key %s: %v", testFilePath.ToURI(), err)
	}
}

func TestExecutorRunLocal(t *testing.T) {
	logger := zaptest.NewLogger(t).Sugar()
	localConfig := LocalExecutorConfig{
		ScriptPath: "./scripts/k8s/offline_store_pandas_runner.py",
	}
	serialized, err := localConfig.Serialize()
	if err != nil {
		t.Fatalf("Error serializing local executor configuration: %v", err)
	}
	executor, err := NewLocalExecutor(Config(serialized), logger)
	if err != nil {
		t.Fatalf("Error creating new Local Executor: %v", err)
	}
	mydir, err := os.Getwd()
	if err != nil {
		t.Fatalf("could not get working directory")
	}

	sqlEnvVars := map[string]string{
		"MODE":                "local",
		"OUTPUT_URI":          fmt.Sprintf(`%s/scripts/k8s/tests/test_files/output/local_test`, mydir),
		"SOURCES":             fmt.Sprintf("%s/scripts/k8s/tests/test_files/inputs/transaction_short/part-00000-9d3cb5a3-4b9c-4109-afa3-a75759bfcf89-c000.snappy.parquet", mydir),
		"TRANSFORMATION_TYPE": "sql",
		"TRANSFORMATION":      "SELECT * FROM source_0 LIMIT 1",
	}
	if err := executor.ExecuteScript(sqlEnvVars, nil); err != nil {
		t.Fatalf("Failed to execute pandas script: %v", err)
	}
}

func TestNewConfig(t *testing.T) {
	err := godotenv.Load("../.env")

	k8sConfig := pc.K8sConfig{
		ExecutorType:   pc.K8s,
		ExecutorConfig: pc.ExecutorConfig{},
		StoreType:      filestore.Azure,
		StoreConfig: &pc.AzureFileStoreConfig{
			AccountName:   helpers.GetEnv("AZURE_ACCOUNT_NAME", ""),
			AccountKey:    helpers.GetEnv("AZURE_ACCOUNT_KEY", ""),
			ContainerName: helpers.GetEnv("AZURE_CONTAINER_NAME", ""),
			Path:          "",
		},
	}
	serialized, err := k8sConfig.Serialize()
	if err != nil {
		t.Fatalf("could not serialize: %v", err)
	}
	provider, err := k8sOfflineStoreFactory(pc.SerializedConfig(serialized))
	if err != nil {
		t.Fatalf("could not get provider")
	}
	_, err = provider.AsOfflineStore()
	if err != nil {
		t.Fatalf("failed to convert store to offline store: %v", err)
	}
}

func Test_parquetIteratorFromReader(t *testing.T) {
	rows := 1000000
	type RowType struct {
		Index  int
		SIndex string
	}

	var buf bytes.Buffer
	w := parquet.NewWriter(&buf)
	var testRows []RowType
	for i := 1; i < rows; i++ {
		row := RowType{
			i,
			fmt.Sprintf("%d", i),
		}
		testRows = append(testRows, row)
		w.Write(row)
	}
	w.Close()

	iter, err := parquetIteratorFromBytes(buf.Bytes())
	if err != nil {
		t.Fatalf(err.Error())
	}
	index := 0
	for {
		value, err := iter.Next()
		if err != nil {
			break
		} else if value == nil && err == nil {
			break
		}
		var result RowType
		mapstructure.Decode(value, &result)
		if result != testRows[index] {
			t.Errorf("Rows not equal %v!=%v\n", value, testRows[index])
		}
		index += 1
	}
}

func testExists(t *testing.T, store FileStore) {
	randomFilePath, err := store.CreateFilePath(uuid.New().String())
	if err != nil {
		t.Fatalf("Could not create random file path: %v", err)
	}
	randomData := []byte(uuid.New().String())
	if err := store.Write(randomFilePath, randomData); err != nil {
		t.Fatalf("Could not write key to filestore: %v", err)
	}
	exists, err := store.Exists(randomFilePath)
	if err != nil {
		t.Fatalf("Could not check that key exists in filestore: %v", err)
	}
	if !exists {
		t.Fatalf("Key written to file store does not exist")
	}
	// cleanup test
	if store.Delete(randomFilePath); err != nil {
		t.Fatalf("error deleting random key: %v", err)
	}
}

func testNotExists(t *testing.T, store FileStore) {
	randomFilePath, err := store.CreateFilePath(uuid.New().String())
	if err != nil {
		t.Fatalf("Could not create random file path: %v", err)
	}
	exists, err := store.Exists(randomFilePath)
	if err != nil {
		t.Fatalf("Could not check that key exists in filestore: %v", err)
	}
	if exists {
		t.Fatalf("Key not written to file store exists")
	}
}

func getMockSchemaAndRecords(length int) (TableSchema, []GenericRecord) {
	schema := TableSchema{
		Columns: []TableColumn{
			{Name: "ID", ValueType: Int},
			{Name: "Name", ValueType: String},
			{Name: "Points", ValueType: Float32},
			{Name: "Score", ValueType: Float64},
			{Name: "Registered", ValueType: Bool},
			{Name: "Created", ValueType: Timestamp},
		},
	}

	records := make([]GenericRecord, length)

	for i := 0; i < length; i++ {
		records[i] = []interface{}{i, uuid.New().String(), float32(i) + 0.1, float64(i) + 0.1, false, time.UnixMilli(int64(i)).UTC()}
	}

	return schema, records
}

func compareStructWithInterface(compareStruct any, compareInterface map[string]interface{}) (bool, error) {
	structValue := reflect.ValueOf(compareStruct)
	structType := structValue.Type()

	for i := 0; i < structValue.NumField(); i++ {
		val, ok := compareInterface[structType.Field(i).Name]
		if !ok {
			return false, fmt.Errorf("submitted struct contains field not in interface: %s", structType.Field(i).Name)
		}
		if val != structValue.Field(i).Interface() {
			return false, fmt.Errorf("submitted struct field value not same as in interface. Expected %v %T, got %v %T", structValue.Field(i).Interface(), structValue.Field(i).Interface(), val, val)
		}
	}
	return true, nil
}

func testServe(t *testing.T, store FileStore) {
	parquetNumRows := 5
	schema, records := getMockSchemaAndRecords(parquetNumRows)
	parquetBytes, err := convertToParquetBytes(schema, records)
	if err != nil {
		t.Fatalf("could not convert struct list to parquet bytes: %v", err)
	}
	randomParquetKey := fmt.Sprintf("%s.parquet", uuid.New().String())
	randomParqetFilePath, err := store.CreateFilePath(randomParquetKey)
	if err != nil {
		t.Fatalf("Could not create random file path: %v", err)
	}
	if err := store.Write(randomParqetFilePath, parquetBytes); err != nil {
		t.Fatalf("Could not write parquet bytes to random key: %v", err)
	}
	iterator, err := store.Serve([]filestore.Filepath{randomParqetFilePath})
	if err != nil {
		t.Fatalf("Could not get parquet iterator: %v", err)
	}
	idx := 0
	for {
		parquetRow, err := iterator.Next()
		parquetRecord := GenericRecord{parquetRow["ID"], parquetRow["Name"], parquetRow["Points"], parquetRow["Score"], parquetRow["Registered"], parquetRow["Created"]}
		idx += 1
		if err != nil {
			t.Fatalf("Error iterating through parquet file: %v", err)
		}
		if parquetRow == nil {
			if idx-1 != parquetNumRows {
				t.Fatalf("Incorrect number of rows in parquet file. Expected %d, got %d", parquetNumRows, idx-1)
			}
			break
		}
		if idx-1 > parquetNumRows {
			t.Fatalf("iterating over more rows than given")
		}
		if !reflect.DeepEqual(records[idx-1], parquetRecord) {
			t.Fatalf("Submitted row and returned struct not identical. Got %v, expected %v", parquetRecord, records[idx-1])
		}
	}
	// cleanup test
	if err := store.Delete(randomParqetFilePath); err != nil {
		t.Fatalf("Could not delete parquet file: %v", err)
	}
}

func testServeDirectory(t *testing.T, store FileStore) {
	parquetNumRows := 5
	parquetNumFiles := 5
	randomDirKey := uuid.New().String()
	randomDirectory, err := store.CreateDirPath(randomDirKey)
	if err != nil {
		t.Fatalf("Could not create random directory: %v", err)
	}
	files := make([]filestore.Filepath, parquetNumFiles)
	records := make([][]GenericRecord, parquetNumFiles)
	for i := 0; i < parquetNumFiles; i++ {
		schema, r := getMockSchemaAndRecords(parquetNumRows)
		records[i] = r
		parquetBytes, err := convertToParquetBytes(schema, records[i])
		if err != nil {
			t.Fatalf("could not convert struct list to parquet bytes: %v", err)
		}
		randomKey := fmt.Sprintf("part000%d%s.parquet", i, uuid.New().String())
		randomPath := fmt.Sprintf("%s/%s", randomDirKey, randomKey)
		randomFilePath, err := store.CreateFilePath(randomPath)
		if err != nil {
			t.Fatalf("Could not create random file path: %v", err)
		}
		if err := store.Write(randomFilePath, parquetBytes); err != nil {
			t.Fatalf("Could not write parquet bytes to path: %v", err)
		}
		files[i] = randomFilePath
	}
	iterator, err := store.Serve(files)
	if err != nil {
		t.Fatalf("Could not get parquet iterator: %v", err)
	}
	totalRows := int64(parquetNumFiles * parquetNumRows)
	idx := int64(0)
	for {
		parquetRow, err := iterator.Next()
		if err != nil {
			t.Fatalf("Error iterating through parquet file: %v", err)
		}
		parquetRecord := GenericRecord{parquetRow["ID"], parquetRow["Name"], parquetRow["Points"], parquetRow["Score"], parquetRow["Registered"], parquetRow["Created"]}
		idx += 1
		if parquetRecord == nil {
			if idx-1 != totalRows {
				t.Fatalf("Incorrect number of rows in parquet file. Expected %d, got %d", totalRows, idx-1)
			}
			break
		}
		if idx-1 > totalRows {
			t.Fatalf("iterating over more rows than given")
		}
		numFile := int((idx - 1) / 5)
		numRow := (idx - 1) % 5
		if numFile >= len(records) {
			break
		}
		if !reflect.DeepEqual(records[numFile][numRow], parquetRecord) {
			t.Fatalf("Submitted row and returned struct not identical. Got %v, expected %v", parquetRecord, records[numFile][numRow])
		}
	}
	// cleanup test
	if err := store.DeleteAll(randomDirectory); err != nil {
		t.Fatalf("Could not delete parquet directory: %v", err)
	}
}

func testDelete(t *testing.T, store FileStore) {
	randomFilePath, err := store.CreateFilePath(uuid.New().String())
	if err != nil {
		t.Fatalf("Could not create random file path: %v", err)
	}
	randomData := []byte(uuid.New().String())
	if err := store.Write(randomFilePath, randomData); err != nil {
		t.Fatalf("Could not write key to filestore: %v", err)
	}
	exists, err := store.Exists(randomFilePath)
	if err != nil {
		t.Fatalf("Could not check that key exists in filestore: %v", err)
	}
	if !exists {
		t.Fatalf("Key written to file store does not exist")
	}
	if err := store.Delete(randomFilePath); err != nil {
		t.Fatalf("Could not delete key from filestore: %v", err)
	}
	exists, err = store.Exists(randomFilePath)
	if err != nil {
		t.Fatalf("Could not check that key exists in filestore: %v", err)
	}
	if exists {
		t.Fatalf("Key deleted from file store exists")
	}

}

func testDeleteAll(t *testing.T, store FileStore) {
	randomListLength := 5
	randomDirKey := uuid.New().String()
	randomDirectory, err := store.CreateDirPath(randomDirKey)
	if err != nil {
		t.Fatalf("Could not create random directory: %v", err)
	}
	randomKeyList := make([]string, randomListLength)
	for i := 0; i < randomListLength; i++ {
		randomKeyList[i] = uuid.New().String()
		randomData := []byte(uuid.New().String())
		randomPath := fmt.Sprintf("%s/%s", randomDirKey, randomKeyList[i])
		randomFilePath, err := store.CreateFilePath(randomPath)
		if err != nil {
			t.Fatalf("Could not create random file path: %v", err)
		}
		if err := store.Write(randomFilePath, randomData); err != nil {
			t.Fatalf("Could not write key to filestore: %v", err)
		}
	}
	for i := 0; i < randomListLength; i++ {
		randomPath := fmt.Sprintf("%s/%s", randomDirKey, randomKeyList[i])
		randomFilePath, err := store.CreateFilePath(randomPath)
		if err != nil {
			t.Fatalf("Could not create random file path: %v", err)
		}
		exists, err := store.Exists(randomFilePath)
		if err != nil {
			t.Fatalf("Could not check that key exists in filestore: %v", err)
		}
		if !exists {
			t.Fatalf("Key written to file store does not exist")
		}
	}
	if err := store.DeleteAll(randomDirectory); err != nil {
		t.Fatalf("Could not delete directory: %v", err)
	}
	for i := 0; i < randomListLength; i++ {
		randomPath := fmt.Sprintf("%s/%s", randomDirectory, randomKeyList[i])
		randomFilePath, err := store.CreateFilePath(randomPath)
		if err != nil {
			t.Fatalf("Could not create random file path: %v", err)
		}
		exists, err := store.Exists(randomFilePath)
		if err != nil {
			t.Fatalf("Could not check that key exists in filestore: %v", err)
		}
		if exists {
			t.Errorf("Key %s still exists", randomPath)
		}
	}

}

func testNewestFile(t *testing.T, store FileStore) {
	// write a bunch of blobs with different timestamps
	randomListLength := 5
	randomDirKey := uuid.New().String()
	randomDirectory, err := store.CreateDirPath(randomDirKey)
	if err != nil {
		t.Fatalf("Could not create random directory: %v", err)
	}
	randomKeyList := make([]string, randomListLength)
	for i := 0; i < randomListLength; i++ {
		randomKeyList[i] = uuid.New().String()
		randomData := []byte(uuid.New().String())
		randomPath := fmt.Sprintf("%s/%s.parquet", randomDirKey, randomKeyList[i])
		randomFilePath, err := store.CreateFilePath(randomPath)
		if err != nil {
			t.Fatalf("Could not create random file path: %v", err)
		}
		if err := store.Write(randomFilePath, randomData); err != nil {
			t.Fatalf("Could not write key to filestore: %v", err)
		}
		time.Sleep(1 * time.Second) // To guarantee ordering of created in metadata follows struct ordering
	}
	newestFile, err := store.NewestFileOfType(randomDirectory, filestore.Parquet)
	if err != nil {
		t.Fatalf("Error getting newest file from directory: %v", err)
	}
	expectedNewestFile := fmt.Sprintf("%s/%s.parquet", randomDirectory.ToURI(), randomKeyList[randomListLength-1])
	if newestFile.ToURI() != expectedNewestFile {
		t.Fatalf("Newest file did not retrieve actual newest file. Expected '%s', got '%s'", expectedNewestFile, newestFile.ToURI())
	}
	// cleanup test
	if err := store.DeleteAll(randomDirectory); err != nil {
		t.Fatalf("Could not delete directory: %v", err)
	}
}

func testNumRows(t *testing.T, store FileStore) {
	parquetNumRows := 5
	schema, records := getMockSchemaAndRecords(parquetNumRows)
	parquetBytes, err := convertToParquetBytes(schema, records)
	if err != nil {
		t.Fatalf("could not convert struct list to parquet bytes: %v", err)
	}
	randomParquetPath, err := store.CreateFilePath(fmt.Sprintf("%s.parquet", uuid.New().String()))
	if err != nil {
		t.Fatalf("Could not create random file path: %v", err)
	}
	if err := store.Write(randomParquetPath, parquetBytes); err != nil {
		t.Fatalf("Could not write parquet bytes to path: %v", err)
	}
	numRows, err := store.NumRows(randomParquetPath)
	if err != nil {
		t.Fatalf("Could not get num rows from parquet file: %v", err)
	}
	if numRows != int64(parquetNumRows) {
		t.Fatalf("Incorrect retrieved num rows from parquet file. Expected %d, got %d", numRows, parquetNumRows)
	}
	// cleanup test
	if err := store.Delete(randomParquetPath); err != nil {
		t.Fatalf("Could not delete parquet file: %v", err)
	}
}

func TestDatabricksInitialization(t *testing.T) {
	host := helpers.GetEnv("DATABRICKS_HOST", "")
	token := helpers.GetEnv("DATABRICKS_ACCESS_TOKEN", "")
	cluster := helpers.GetEnv("DATABRICKS_CLUSTER", "")
	databricksConfig := pc.DatabricksConfig{
		Host:    host,
		Token:   token,
		Cluster: cluster,
	}
	executor, err := NewDatabricksExecutor(databricksConfig)
	if err != nil {
		t.Fatalf("Could not create new databricks client: %v", err)
	}

	azureStoreConfig := &pc.AzureFileStoreConfig{
		AccountName:   helpers.GetEnv("AZURE_ACCOUNT_NAME", ""),
		AccountKey:    helpers.GetEnv("AZURE_ACCOUNT_KEY", ""),
		ContainerName: helpers.GetEnv("AZURE_CONTAINER_NAME", ""),
		// Path:          "testdirectory/testpath",
		Path: "refactor_filepath_tests",
	}
	serializedAzureConfig, err := azureStoreConfig.Serialize()
	if err != nil {
		t.Fatalf("failed to serialize azure store config: %v", err)
	}
	azureFileStore, err := NewSparkAzureFileStore(serializedAzureConfig)
	if err != nil {
		t.Fatalf("failed to create new azure blob store: %v", err)
	}

	if err := executor.InitializeExecutor(azureFileStore); err != nil {
		t.Fatalf("Error initializing executor: %v", err)
	}
}

func TestKubernetesExecutor_isDefaultImage(t *testing.T) {
	logger := zaptest.NewLogger(t).Sugar()
	type fields struct {
		logger *zap.SugaredLogger
		image  string
	}
	tests := []struct {
		name    string
		fields  fields
		want    bool
		wantErr bool
	}{
		{"Valid Base", fields{logger, config.PandasBaseImage}, true, false},
		{"Valid Version", fields{logger, fmt.Sprintf("%s:%s", config.PandasBaseImage, "latest")}, true, false},
		{"Invalid Base", fields{logger, "my-docker/image"}, false, false},
		{"Invalid Base With Tag", fields{logger, fmt.Sprintf("%s:%s", "my-docker/image", "latest")}, false, false},
		{"Invalid Extended Name", fields{logger, fmt.Sprintf("%s%s", config.PandasBaseImage, "xyz")}, false, false},
		{"Invalid Name Format", fields{logger, "abc...fsdf"}, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kube := KubernetesExecutor{
				logger: tt.fields.logger,
				image:  tt.fields.image,
			}
			got, err := kube.isDefaultImage()
			if (err != nil) != tt.wantErr {
				t.Errorf("checkArgs() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("isDefaultImage() = %v, want %v\n image: %s", got, tt.want, tt.fields.image)
			}
		})
	}
}

func TestKExecutorConfig_getImage(t *testing.T) {
	type fields struct {
		DockerImage string
	}
	tests := []struct {
		name   string
		fields fields
		want   string
	}{
		{"No Image", fields{""}, config.PandasBaseImage},
		{"Custom Image", fields{"my-custom/image"}, "my-custom/image"},
		{"Base Image", fields{config.PandasBaseImage}, config.PandasBaseImage},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := pc.ExecutorConfig{
				DockerImage: tt.fields.DockerImage,
			}
			if got := c.GetImage(); got != tt.want {
				t.Errorf("getImage() = %v, want %v", got, tt.want)
			}
		})
	}
}

type dummyArgs struct{}

func (arg dummyArgs) Format() map[string]string {
	return nil
}

func (arg dummyArgs) Type() metadata.TransformationArgType {
	return metadata.NoArgs
}

func TestK8sOfflineStore_checkArgs(t *testing.T) {

	type fields struct {
		executor     Executor
		store        FileStore
		logger       *zap.SugaredLogger
		query        *pandasOfflineQueries
		BaseProvider BaseProvider
	}
	f := fields{
		&KubernetesExecutor{},
		&AzureFileStore{},
		zaptest.NewLogger(t).Sugar(),
		nil,
		BaseProvider{},
	}
	type args struct {
		args metadata.TransformationArgs
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		want    metadata.KubernetesArgs
		wantErr bool
	}{
		{"Empty Args", f, args{metadata.KubernetesArgs{}}, metadata.KubernetesArgs{}, false},
		{"Empty With Docker Image", f, args{metadata.KubernetesArgs{DockerImage: "my/docker:image"}}, metadata.KubernetesArgs{DockerImage: "my/docker:image"}, false},
		{"Invalid Args", f, args{dummyArgs{}}, metadata.KubernetesArgs{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k8s := &K8sOfflineStore{
				executor:     tt.fields.executor,
				store:        tt.fields.store,
				logger:       tt.fields.logger,
				query:        tt.fields.query,
				BaseProvider: tt.fields.BaseProvider,
			}
			got, err := k8s.checkArgs(tt.args.args)
			if (err != nil) != tt.wantErr {
				t.Errorf("checkArgs() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("checkArgs() got = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestKubernetesExecutor_setCustomImage(t *testing.T) {
	type fields struct {
		logger *zap.SugaredLogger
		image  string
	}
	type args struct {
		image string
	}
	tests := []struct {
		name     string
		fields   fields
		args     args
		expected string
	}{
		{"Empty Image", fields{nil, ""}, args{""}, ""},
		{"Default Image", fields{nil, "test/image"}, args{""}, "test/image"},
		{"Override Image", fields{nil, "test/image"}, args{"override/image"}, "override/image"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kube := &KubernetesExecutor{
				logger: tt.fields.logger,
				image:  tt.fields.image,
			}
			kube.setCustomImage(tt.args.image)
			if kube.image != tt.expected {
				t.Errorf("Expected image %s, got %s", tt.expected, kube.image)
			}
		})
	}
}

func TestTrainingSetOrder(t *testing.T) {

	type RowType struct {
		Feature1     string
		Feature2     string
		Feature3     string
		Feature4     string
		Label__field string
	}
	rows := 1000

	featureColNames := []string{"Feature1", "Feature2", "Feature3", "Feature4"}
	labelColName := "Label__field" // Parquet Label name format

	getStructValueByName := func(s interface{}, f string) interface{} {
		r := reflect.ValueOf(s)
		return reflect.Indirect(r).FieldByName(f).String()
	}

	featuresInOrder := func(index int, features []interface{}, testRows []RowType) bool {
		for j, v := range features {
			if getStructValueByName(testRows[index], featureColNames[j]) != v {
				return false
			}
		}
		return true
	}

	createTestRows := func(w *parquet.Writer) []RowType {
		var testRows []RowType
		for i := 1; i < rows; i++ {
			row := RowType{
				fmt.Sprintf("feat1 %d", rand.Int()),
				fmt.Sprintf("feat2 %d", rand.Int()),
				fmt.Sprintf("feat3 %d", rand.Int()),
				fmt.Sprintf("feat4 %d", rand.Int()),
				fmt.Sprintf("label %d", rand.Int()),
			}
			testRows = append(testRows, row)
			w.Write(row)
		}
		w.Close()
		return testRows
	}

	var buf bytes.Buffer
	w := parquet.NewWriter(&buf)

	testRows := createTestRows(w)

	iter, err := parquetIteratorFromBytes(buf.Bytes())
	if err != nil {
		t.Fatalf(err.Error())
	}
	tsIterator := FileStoreTrainingSet{
		iter: iter,
	}

	i := 0
	for tsIterator.Next() {
		if !featuresInOrder(i, tsIterator.Features(), testRows) {
			t.Errorf("Expected features: %v, got %v", testRows[i], tsIterator.Features())
		}
		if getStructValueByName(testRows[i], labelColName) != tsIterator.Label() {
			t.Errorf("Expected label: %v, got %v", getStructValueByName(testRows[i], labelColName), tsIterator.Label())
		}
		i++
	}
}

func TestParquetIterator_vector32(t *testing.T) {
	data, err := ioutil.ReadFile("test_files/vector32.parquet")
	if err != nil {
		t.Fatalf("could not read vector32 parquet file: %v", err)
	}
	iter, err := parquetIteratorFromBytes(data)
	if err != nil {
		t.Fatalf("could not create parquet iterator: %v", err)
	}
	for {
		v, err := iter.Next()
		if err != nil {
			t.Fatalf("could not get next row: %v", err)
		}
		value := v["value"]
		if value == nil {
			break
		}
		mapValue, ok := value.(map[string]interface{})
		if !ok {
			t.Fatalf("could not cast type: %T to map[string]interface{}", value)
		}
		list, ok := mapValue["list"]
		if !ok {
			t.Fatalf("could not find list in value: %v", value)
		}
		elementsSlice, ok := list.([]interface{})
		if !ok {
			t.Fatalf("could not cast type: %T to []interface{}", list)
		}
		vector32 := make([]float32, len(elementsSlice))
		for i, e := range elementsSlice {
			m, ok := e.(map[string]interface{})
			if !ok {
				t.Fatalf("could not cast type: %T to map[string]interface{}", e)
			}
			switch element := m["element"].(type) {
			case float32:
				vector32[i] = element
			case float64:
				vector32[i] = float32(element)
			default:
				t.Fatalf("could not cast element type: %T to float32", element)
			}
		}
	}
}

func convertToParquetBytes(schema TableSchema, list []GenericRecord) ([]byte, error) {
	if len(list) == 0 {
		return nil, fmt.Errorf("list is empty")
	}
	parquetRecords := schema.ToParquetRecords(list)
	parquetSchema := parquet.SchemaOf(schema.Interface())
	buf := new(bytes.Buffer)
	err := parquet.Write[any](
		buf,
		parquetRecords,
		parquetSchema,
	)
	if err != nil {
		return nil, fmt.Errorf("could not write parquet file to bytes: %v", err)
	}
	return buf.Bytes(), nil
}
