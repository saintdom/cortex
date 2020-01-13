/*
Copyright 2019 Cortex Labs, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package operator

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/cortexlabs/cortex/pkg/lib/aws"
	"github.com/cortexlabs/cortex/pkg/lib/cast"
	"github.com/cortexlabs/cortex/pkg/lib/errors"
	"github.com/cortexlabs/cortex/pkg/lib/k8s"
	"github.com/cortexlabs/cortex/pkg/lib/parallel"
	"github.com/cortexlabs/cortex/pkg/lib/pointer"
	"github.com/cortexlabs/cortex/pkg/lib/urls"
	"github.com/cortexlabs/cortex/pkg/types/userconfig"
	"github.com/cortexlabs/cortex/pkg/operator/config"
	kresource "k8s.io/apimachinery/pkg/api/resource"
	kunstructured "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

var _apiValidation = &cr.StructValidation{
	StructFieldValidations: []*cr.StructFieldValidation{
		{
			StructField: "Name",
			StringValidation: &cr.StringValidation{
				Required: true,
				DNS1035:  true,
			},
		},
		{
			StructField: "Endpoint",
			StringPtrValidation: &cr.StringPtrValidation{
				Validator: urls.ValidateEndpoint,
			},
		},
		{
			StructField: "Tracker",
			StructValidation: &cr.StructValidation{
				DefaultNil: true,
				StructFieldValidations: []*cr.StructFieldValidation{
					{
						StructField:         "Key",
						StringPtrValidation: &cr.StringPtrValidation{},
					},
					{
						StructField: "ModelType",
						StringValidation: &cr.StringValidation{
							Required:      false,
							AllowEmpty:    true,
							AllowedValues: userconfig.ModelTypeStrings(),
						},
						Parser: func(str string) (interface{}, error) {
							return userconfig.ModelTypeFromString(str), nil
						},
					},
				},
			},
		},
		_predictorValidation,
		_computeFieldValidation,
	},
}

var _predictorValidation = &cr.StructFieldValidation{
	StructField: "Predictor",
	StructValidation: &cr.StructValidation{
		Required: true,
		StructFieldValidations: []*cr.StructFieldValidation{
			{
				StructField: "Type",
				StringValidation: &cr.StringValidation{
					Required:      true,
					AllowedValues: userconfig.PredictorTypeStrings(),
				},
				Parser: func(str string) (interface{}, error) {
					return userconfig.PredictorTypeFromString(str), nil
				},
			},
			{
				StructField: "Path",
				StringValidation: &cr.StringValidation{
					Required: true,
				},
			},
			{
				StructField: "Model",
				StringPtrValidation: &cr.StringPtrValidation{
					Validator: cr.S3PathValidator(),
				},
			},
			{
				StructField: "PythonPath",
				StringPtrValidation: &cr.StringPtrValidation{
					AllowEmpty: true,
					Validator: func(path string) (string, error) {
						return s.EnsureSuffix(path, "/"), nil
					},
				},
			},
			{
				StructField: "Config",
				InterfaceMapValidation: &cr.InterfaceMapValidation{
					StringKeysOnly: true,
					AllowEmpty:     true,
					Default:        map[string]interface{}{},
				},
			},
			{
				StructField: "Env",
				StringMapValidation: &cr.StringMapValidation{
					Default:    map[string]string{},
					AllowEmpty: true,
				},
			},
			{
				StructField:         "SignatureKey",
				StringPtrValidation: &cr.StringPtrValidation{},
			},
		},
	},
}

var _computeFieldValidation = &cr.StructFieldValidation{
	StructField: "Compute",
	StructValidation: &cr.StructValidation{
		StructFieldValidations: []*cr.StructFieldValidation{
			{
				StructField: "MinReplicas",
				Int32Validation: &cr.Int32Validation{
					Default:     1,
					GreaterThan: pointer.Int32(0),
				},
			},
			{
				StructField: "MaxReplicas",
				Int32Validation: &cr.Int32Validation{
					Default:     100,
					GreaterThan: pointer.Int32(0),
				},
			},
			{
				StructField:  "InitReplicas",
				DefaultField: "MinReplicas",
				Int32Validation: &cr.Int32Validation{
					GreaterThan: pointer.Int32(0),
				},
			},
			{
				StructField: "TargetCPUUtilization",
				Int32Validation: &cr.Int32Validation{
					Default:     80,
					GreaterThan: pointer.Int32(0),
				},
			},
			{
				StructField: "CPU",
				StringValidation: &cr.StringValidation{
					Default:     "200m",
					CastNumeric: true,
				},
				Parser: k8s.QuantityParser(&k8s.QuantityValidation{
					GreaterThan: k8s.QuantityPtr(kresource.MustParse("0")),
				}),
			},
			{
				StructField: "Mem",
				StringPtrValidation: &cr.StringPtrValidation{
					Default: nil,
				},
				Parser: k8s.QuantityParser(&k8s.QuantityValidation{
					GreaterThan: k8s.QuantityPtr(kresource.MustParse("0")),
				}),
			},
			{
				StructField: "GPU",
				Int64Validation: &cr.Int64Validation{
					Default:              0,
					GreaterThanOrEqualTo: pointer.Int64(0),
				},
			},
		},
	},
}

func ExtractAPIConfigs(configBytes []byte, projectFileMap map[string][]byte, filePath string) ([]*API, error) {
	var err error

	configData, err := cr.ReadYAMLBytes(configBytes)
	if err != nil {
		return nil, errors.Wrap(err, filePath)
	}

	configDataSlice, ok := cast.InterfaceToStrInterfaceMapSlice(configData)
	if !ok {
		return nil, errors.Wrap(ErrorMalformedConfig(), filePath)
	}

	apis := make([]*API, len(configDataSlice))
	for i, data := range configDataSlice {
		api := &API{}
		errs := cr.Struct(api, data, apiValidation)
		if errors.HasErrors(errs) {
			name, _ := data[NameKey].(string)
			return nil, errors.Wrap(errors.FirstError(errs...), userconfig.IdentifyAPI(filePath, name, i))
		}

		api.Index = i
		api.FilePath = filePath
		apis = append(apis, api)
	}

	if err := validateAPIs(apis, projectFileMap); err != nil {
		return nil, err
	}

	return apis, nil
}

func validateAPIs(apis []*userconfig.API, projectFileMap map[string][]byte) error {
	if len(apis) == 0 {
		return ErrorNoAPIs()
	}

	dups := findDuplicateNames(apis)
	if len(dups) > 0 {
		return ErrorDuplicateName(dups)
	}

	maxMem, virtualServices, err := getValidationK8sResources()
	if err != nil {
		return err
	}

	for _, api := range apis {
		if err := validateAPI(api, projectFileMap, virtualServices, maxMem); err != nil {
			return err
		}
	}
}

func validateAPI(
	api *userconfig.API,
	projectFileMap map[string][]byte,
	virtualServices []kunstructured.Unstructured,
	maxMem *kresource.Quantity,
) error {

	if api.Endpoint == nil {
		api.Endpoint = pointer.String("/" + api.Name)
	}

	if err := validatePredictor(api.Predictor, projectFileMap); err != nil {
		return errors.Wrap(err, api.Identify(), PredictorKey)
	}

	if err := validateCompute(api.Compute, maxMem); err != nil {
		return errors.Wrap(err, api.Identify(), ComputeKey)
	}

	if err := validateEndpointCollisions(api, virtualServices); err != nil {
		return err
	}

	return nil
}

func validatePredictor(predictor *userconfig.Predictor, projectFileMap map[string][]byte) error {
	switch predictor.Type {
	case userconfig.PythonPredictorType:
		if err := validatePythonPredictor(predictor); err != nil {
			return err
		}
	case userconfig.TensorFlowPredictorType:
		if err := validateTensorFlowPredictor(predictor); err != nil {
			return err
		}
	case userconfig.ONNXPredictorType:
		if err := validateONNXPredictor(predictor); err != nil {
			return err
		}
	}

	if _, ok := projectFileMap[predictor.Path]; !ok {
		return errors.Wrap(ErrorImplDoesNotExist(predictor.Path), PathKey)
	}

	if predictor.PythonPath != nil {
		if err := validatePythonPath(*predictor.PythonPath, projectFileMap); err != nil {
			return errors.Wrap(err, PythonPathKey)
		}
	}

	return nil
}

func validatePythonPredictor(predictor *userconfig.Predictor) error {
	if predictor.SignatureKey != nil {
		return ErrorFieldNotSupportedByPredictorType(SignatureKeyKey, PythonPredictorType)
	}

	if predictor.Model != nil {
		return ErrorFieldNotSupportedByPredictorType(ModelKey, PythonPredictorType)
	}

	return nil
}

func validateTensorFlowPredictor(predictor *userconfig.Predictor) error {
	if predictor.Model == nil {
		return ErrorFieldMustBeDefinedForPredictorType(ModelKey, TensorFlowPredictorType)
	}

	model := *predictor.Model

	awsClient, err := aws.NewFromS3Path(model, false)
	if err != nil {
		return err
	}

	if strings.HasSuffix(model, ".zip") {
		if ok, err := awsClient.IsS3PathFile(model); err != nil || !ok {
			return errors.Wrap(ErrorS3FileNotFound(model), ModelKey)
		}
	} else {
		path, err := getTFServingExportFromS3Path(model, awsClient)
		if path == "" || err != nil {
			return errors.Wrap(ErrorInvalidTensorFlowDir(model), ModelKey)
		}
		predictor.Model = pointer.String(path)
	}

	return nil
}

func validateONNXPredictor(predictor *userconfig.Predictor) error {
	if predictor.Model == nil {
		return ErrorFieldMustBeDefinedForPredictorType(ModelKey, ONNXPredictorType)
	}

	model := *predictor.Model

	awsClient, err := aws.NewFromS3Path(model, false)
	if err != nil {
		return err
	}

	if ok, err := awsClient.IsS3PathFile(model); err != nil || !ok {
		return errors.Wrap(ErrorS3FileNotFound(model), ModelKey)
	}

	if predictor.SignatureKey != nil {
		return ErrorFieldNotSupportedByPredictorType(SignatureKeyKey, ONNXPredictorType)
	}
}

func getTFServingExportFromS3Path(path string, awsClient *aws.Client) (string, error) {
	if isValidTensorFlowS3Directory(path, awsClient) {
		return path, nil
	}

	bucket, prefix, err := aws.SplitS3Path(path)
	if err != nil {
		return "", err
	}
	prefix = s.EnsureSuffix(prefix, "/")

	resp, _ := awsClient.S3.ListObjects(&s3.ListObjectsInput{
		Bucket: &bucket,
		Prefix: &prefix,
	})

	highestVersion := int64(0)
	var highestPath string
	for _, key := range resp.Contents {
		if !strings.HasSuffix(*key.Key, "saved_model.pb") {
			continue
		}

		keyParts := strings.Split(*key.Key, "/")
		versionStr := keyParts[len(keyParts)-1]
		version, err := strconv.ParseInt(versionStr, 10, 64)
		if err != nil {
			version = 0
		}

		possiblePath := "s3://" + filepath.Join(bucket, filepath.Join(keyParts[:len(keyParts)-1]...))
		if version >= highestVersion && IsValidTensorFlowS3Directory(possiblePath, awsClient) {
			highestVersion = version
			highestPath = possiblePath
		}
	}

	return highestPath, nil
}

// IsValidTensorFlowS3Directory checks that the path contains a valid S3 directory for TensorFlow models
// Must contain the following structure:
// - 1523423423/ (version prefix, usually a timestamp)
// 		- saved_model.pb
//		- variables/
//			- variables.index
//			- variables.data-00000-of-00001 (there are a variable number of these files)
func isValidTensorFlowS3Directory(path string, awsClient *aws.Client) bool {
	if valid, err := awsClient.IsS3PathFile(
		aws.S3PathJoin(path, "saved_model.pb"),
		aws.S3PathJoin(path, "variables/variables.index"),
	); err != nil || !valid {
		return false
	}

	if valid, err := awsClient.IsS3PathPrefix(
		aws.S3PathJoin(path, "variables/variables.data-00000-of"),
	); err != nil || !valid {
		return false
	}
	return true
}

func validatePythonPath(pythonPath string, projectFileMap map[string][]byte) error {
	validPythonPath := false
	for fileKey := range projectFileMap {
		if strings.HasPrefix(fileKey, pythonPath) {
			validPythonPath = true
			break
		}
	}
	if !validPythonPath {
		return ErrorImplDoesNotExist(pythonPath)
	}
	return nil
}

func validateCompute(compute *Compute, maxMem *kresource.Quantity) error {
	if compute.MinReplicas > compute.MaxReplicas {
		return ErrorMinReplicasGreaterThanMax(compute.MinReplicas, compute.MaxReplicas)
	}

	if compute.InitReplicas > compute.MaxReplicas {
		return ErrorInitReplicasGreaterThanMax(compute.InitReplicas, compute.MaxReplicas)
	}

	if compute.InitReplicas < compute.MinReplicas {
		return ErrorInitReplicasLessThanMin(compute.InitReplicas, compute.MinReplicas)
	}

	if err := validateAvailableCompute(compute, maxMem); err != nil {
		return err
	}

	return nil
}

func validateAvailableCompute(compute *userconfig.Compute, maxMem *kresource.Quantity) error {
	maxMem.Sub(_cortexMemReserve)

	maxCPU := config.Cluster.InstanceMetadata.CPU
	maxCPU.Sub(_cortexCPUReserve)

	maxGPU := config.Cluster.InstanceMetadata.GPU
	if maxGPU > 0 {
		// Reserve resources for nvidia device plugin daemonset
		maxCPU.Sub(_nvidiaCPUReserve)
		maxMem.Sub(_nvidiaMemReserve)
	}

	if maxCPU.Cmp(compute.CPU.Quantity) < 0 {
		return ErrorNoAvailableNodeComputeLimit("CPU", compute.CPU.String(), maxCPU.String())
	}
	if compute.Mem != nil {
		if maxMem.Cmp(compute.Mem.Quantity) < 0 {
			return ErrorNoAvailableNodeComputeLimit("Memory", compute.Mem.String(), maxMem.String())
		}
	}
	gpu := compute.GPU
	if gpu > maxGPU {
		return ErrorNoAvailableNodeComputeLimit("GPU", fmt.Sprintf("%d", gpu), fmt.Sprintf("%d", maxGPU))
	}
	return nil
}

func validateEndpointCollisions(api *userconfig.API, virtualServices []kunstructured.Unstructured) error {
	for _, virtualService := range virtualServices {
		gateways, err := k8s.ExtractVirtualServiceGateways(&virtualService)
		if err != nil {
			return err
		}
		if !gateways.Has("apis-gateway") {
			continue
		}

		endpoints, err := k8s.ExtractVirtualServiceEndpoints(&virtualService)
		if err != nil {
			return err
		}

		for endpoint := range endpoints {
			if endpoint == *api.Endpoint && virtualService.Labels["apiName"] != api.Name {
				return errors.Wrap(ErrorDuplicateEndpointOtherDeployment(virtualService.Labels["apiName"]), api.Identify(), userconfig.EndpointKey, endpoint)
			}
		}
	}

	return nil
}

func findDuplicateNames(apis []*userconfig.API) []*API {
	names := make(map[string][]*userconfig.API)

	for _, api := range apis {
		names[api.Name] = append(names[api.Name], api)
	}

	for name := range names {
		if len(names[name]) > 1 {
			return names[name]
		}
	}

	return nil
}

func getValidationK8sResources() (
	[]kunstructured.Unstructured,
	*kresource.Quantity,
	error,
) {

	var virtualServices *kunstructured.Unstructured
	var maxMem *kresource.Quantity

	err := parallel.RunFirstErr(
		func() error {
			var err error
			virtualServices, err = config.Kubernetes.ListVirtualServices("default", nil)
			return err
		},
		func() error {
			var err error
			maxMem, err = updateMemoryCapacityConfigMap()
			if err != nil {
				return errors.Wrap(err, "validating memory constraint")
			}
		},
	)

	return virtualServices, maxMem, err
}
