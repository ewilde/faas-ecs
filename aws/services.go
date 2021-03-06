package aws

import (
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/ewilde/faas-fargate/system"
	"github.com/ewilde/faas-fargate/types"
	"github.com/openfaas/faas/gateway/requests"
	log "github.com/sirupsen/logrus"
)

const servicePrefix = "openfaas-"

var clusterID string
var subnetsFunc = &sync.Once{}
var subnets []*string

func init() {
	clusterID = system.GetEnv("cluster_name", "openfaas")
}

// FindECSServiceArn based on the serviceName finds a matching service, returning it's arn.
func FindECSServiceArn(serviceName string) (*string, error) {
	services, err := ecsClient.ListServices(&ecs.ListServicesInput{
		Cluster: ClusterID(),
	})

	if err != nil {
		return nil, err
	}

	var serviceArn *string
	for _, item := range services.ServiceArns {
		if strings.Contains(aws.StringValue(item), serviceName) {
			serviceArn = item
		}
	}

	return serviceArn, nil
}

// UpdateOrCreateECSService either creates an new service or updates an existing one if matched based on the
// service name in the request
func UpdateOrCreateECSService(
	taskDefinition *ecs.TaskDefinition,
	request requests.CreateFunctionRequest,
	cfg *types.DeployHandlerConfig) (*ecs.Service, error) {

	serviceArn, err := FindECSServiceArn(request.Service)
	if err != nil {
		log.Errorln(fmt.Sprintf("Could not find service with name %s.", request.Service), err)
		return nil, err
	}

	if serviceArn != nil {
		service, err := ecsClient.UpdateService(&ecs.UpdateServiceInput{
			Cluster:        ClusterID(),
			Service:        serviceArn,
			DesiredCount:   getMinReplicaCount(request.Labels),
			TaskDefinition: taskDefinition.TaskDefinitionArn,
		})

		if err != nil {
			log.Errorln(fmt.Sprintf("Error updating service %s. ", request.Service), err)
			return nil, err
		}

		return service.Service, err
	}

	registryArn, err := ensureServiceRegistrationExists(request.Service, cfg.VpcID)
	if err != nil {
		log.Errorln(fmt.Sprintf("Error creating registry for service %s. ", request.Service), err)
		return nil, err
	}

	// see: https://docs.aws.amazon.com/cli/latest/reference/ecs/create-service.html
	result, err := ecsClient.CreateService(&ecs.CreateServiceInput{
		Cluster:        ClusterID(),
		ServiceName:    aws.String(ServiceNameFromFunctionName(request.Service)),
		TaskDefinition: taskDefinition.TaskDefinitionArn,
		LaunchType:     aws.String("FARGATE"),
		DesiredCount:   getMinReplicaCount(request.Labels),
		NetworkConfiguration: &ecs.NetworkConfiguration{
			AwsvpcConfiguration: &ecs.AwsVpcConfiguration{
				AssignPublicIp: aws.String(cfg.AssignPublicIP),
				Subnets:        awsSubnet(ec2Client, cfg.SubnetIDs, cfg.VpcID),
				SecurityGroups: []*string{aws.String(cfg.SecurityGroupID)},
			},
		},
		ServiceRegistries: []*ecs.ServiceRegistry{
			{
				RegistryArn: aws.String(registryArn),
			},
		},
	})

	if err != nil {
		log.Errorln(fmt.Sprintf("Error creating service %s. Using subnets from configuration: %s",
			request.Service, cfg.SubnetIDs), err)
		return nil, err
	}

	return result.Service, nil
}

// DeleteECSService remove the service with the supplied name
func DeleteECSService(
	serviceName string,
	cfg *types.DeployHandlerConfig) error {
	serviceArn, err := FindECSServiceArn(serviceName)
	if err != nil {
		return fmt.Errorf("could not find service matching %s. %v", serviceName, err)
	}

	if serviceArn == nil {
		return fmt.Errorf("can not delete a function, no function found matching %s", serviceName)
	}

	services, err := ecsClient.DescribeServices(&ecs.DescribeServicesInput{Cluster: ClusterID(), Services: []*string{serviceArn}})
	if err != nil {
		return fmt.Errorf("could not describe service %s. %v", aws.StringValue(serviceArn), err)
	}

	if *services.Services[0].DesiredCount > 0 {
		ecsClient.UpdateService(&ecs.UpdateServiceInput{
			Cluster:      ClusterID(),
			Service:      serviceArn,
			DesiredCount: aws.Int64(0)})
	}

	// do this async it takes quite a long time
	go func() {
		err = deleteServiceRegistration(serviceName, cfg.VpcID)
		if err != nil {
			log.Errorf("error deleting service discovery registration for %s. %v", serviceName, err)
		}
	}()

	result, err := ecsClient.DeleteService(&ecs.DeleteServiceInput{Cluster: ClusterID(), Service: serviceArn})
	if err != nil {
		return fmt.Errorf("error deleting service %s arn: %s. %v", serviceName, aws.StringValue(serviceArn), err)
	}

	log.Infof("Successfully deleted service %s.", serviceName)

	err = DeleteTaskRevision(serviceName)
	if err != nil {
		return fmt.Errorf("error deleting task revision for service %s arn: %s. %v", serviceName, aws.StringValue(serviceArn), err)
	}

	log.Debugf("deleting function %s result: %s", serviceName, result.String())
	return nil
}

// UpdateECSServiceDesiredCount update the service desired count
func UpdateECSServiceDesiredCount(
	serviceName string,
	desiredCount int) (*ecs.Service, error) {

	serviceArn, err := FindECSServiceArn(serviceName)
	if err != nil {
		log.Errorln(fmt.Sprintf("could not find service with name %s.", serviceName), err)
		return nil, err
	}

	if serviceArn == nil {
		return nil, fmt.Errorf("could not find service %s", serviceName)
	}

	service, err := ecsClient.UpdateService(&ecs.UpdateServiceInput{
		Cluster:      ClusterID(),
		Service:      serviceArn,
		DesiredCount: aws.Int64(int64(desiredCount)),
	})

	if err != nil {
		return nil, err
	}

	return service.Service, nil
}

// ClusterID returns the configured cluster ID
func ClusterID() *string {
	return aws.String(clusterID)
}

// GetServiceList returns the list of OpenFaas functions running
func GetServiceList() ([]requests.Function, error) {
	var functions []requests.Function

	services, err := getServices()
	if err != nil {
		return nil, err
	}

	var serviceNames []*string
	for _, item := range services {
		if !IsFaasService(item) {
			continue
		}

		serviceNames = append(serviceNames, ServiceNameFromArn(item))
	}

	if len(serviceNames) == 0 {
		return functions, nil
	}

	for len(serviceNames) > 0 {
		describe := serviceNames
		if len(serviceNames) > 10 {
			describe = serviceNames[0:10]
			serviceNames = serviceNames[10:]
		} else {
			serviceNames = serviceNames[len(serviceNames):]
		}

		details, err := ecsClient.DescribeServices(&ecs.DescribeServicesInput{Services: describe, Cluster: ClusterID()})
		if err != nil {
			return nil, err
		}

		for _, item := range details.Services {
			task, err := ecsClient.DescribeTaskDefinition(&ecs.DescribeTaskDefinitionInput{TaskDefinition: item.TaskDefinition})
			if err != nil {
				return nil, err
			}

			function := requests.Function{
				Name:              ServiceNameForDisplay(item.ServiceName),
				Replicas:          uint64(*item.RunningCount),
				Image:             aws.StringValue(task.TaskDefinition.ContainerDefinitions[0].Image),
				AvailableReplicas: uint64(*item.DesiredCount), // TODO find out what this property relates to
				InvocationCount:   0,
				Labels:            nil,
			}

			functions = append(functions, function)
		}
	}

	return functions, nil
}

// IsFaasService returns true if the service is an OpenFaaS function
func IsFaasService(arn *string) bool {
	return strings.Contains(aws.StringValue(arn), servicePrefix)
}

// ServiceNameFromArn calculated the service name from the service arn
func ServiceNameFromArn(arn *string) *string {
	return aws.String(strings.Split(*arn, "/")[1])
}

// ServiceNameForDisplay returns the service name shown to the user
func ServiceNameForDisplay(name *string) string {
	return strings.TrimPrefix(*name, servicePrefix)
}

// ServiceNameFromFunctionName returns the aws faargate service name based on the OpenFaaS function name
func ServiceNameFromFunctionName(functionName string) string {
	return servicePrefix + functionName
}

func awsSubnet(client *ec2.EC2, subnetIds string, vpcID string) []*string {

	subnetsFunc.Do(func() {
		if subnetIds == "" {
			log.Debugf("Searching for subnets using vpc id %s", vpcID)
			result, err := client.DescribeSubnets(&ec2.DescribeSubnetsInput{
				Filters: []*ec2.Filter{
					{
						Name: aws.String("vpc-id"),
						Values: []*string{
							aws.String(vpcID),
						},
					},
				},
			})
			if err == nil {
				for _, item := range result.Subnets {
					subnets = append(subnets, item.SubnetId)
				}
			}
		} else {
			log.Debugf("Searching for subnets using list of ids %s", subnetIds)
			subnetIds := strings.Split(subnetIds, ",")
			for _, item := range subnetIds {
				subnets = append(subnets, aws.String(item))
			}
		}
	})

	return subnets
}

func getServices() ([]*string, error) {
	var result []*string
	var next *string

	for {
		services, err := ecsClient.ListServices(
			&ecs.ListServicesInput{
				Cluster:   ClusterID(),
				NextToken: next,
			})
		if err != nil {
			return nil, err
		}

		result = append(result, services.ServiceArns...)
		next = services.NextToken
		if next == nil {
			break
		}
	}

	return result, nil
}

func getMinReplicaCount(labels *map[string]string) *int64 {
	if labels == nil {
		return aws.Int64(1)
	}

	m := *labels
	if value, exists := m["com.openfaas.scale.min"]; exists {
		minReplicas, err := strconv.Atoi(value)
		if err == nil && minReplicas > 0 {
			return aws.Int64(int64(minReplicas))
		}

		log.Errorln(err)
	}

	return aws.Int64(1)
}
