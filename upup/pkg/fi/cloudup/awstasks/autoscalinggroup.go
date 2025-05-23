/*
Copyright 2019 The Kubernetes Authors.

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

package awstasks

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	autoscalingtypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"k8s.io/klog/v2"

	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/upup/pkg/fi/cloudup/awsup"
	"k8s.io/kops/upup/pkg/fi/cloudup/terraform"
	"k8s.io/kops/upup/pkg/fi/cloudup/terraformWriter"
	"k8s.io/kops/util/pkg/maps"
)

const (
	// CloudTagInstanceGroupRolePrefix is a cloud tag that defines the instance role
	CloudTagInstanceGroupRolePrefix = "k8s.io/role/"

	// Auto Scaling group API operations limits
	// https://docs.aws.amazon.com/autoscaling/ec2/userguide/ec2-auto-scaling-quotas.html
	attachLoadBalancerTargetGroupsMaxItems = 10
	detachLoadBalancerTargetGroupsMaxItems = 10
)

// AutoscalingGroup provides the definition for a autoscaling group in aws
// +kops:fitask
type AutoscalingGroup struct {
	// Name is the name of the ASG
	Name *string
	// Lifecycle is the resource lifecycle
	Lifecycle fi.Lifecycle

	// Granularity specifys the granularity of the metrics
	Granularity *string
	// InstanceProtection makes new instances in an autoscaling group protected from scale in
	InstanceProtection *bool
	// LaunchTemplate is the launch template for the asg
	LaunchTemplate *LaunchTemplate
	// LoadBalancers is a list of elastic load balancer names to add to the autoscaling group
	LoadBalancers []*ClassicLoadBalancer
	// MaxInstanceLifetime is the maximum amount of time, in seconds, that an instance can be in service.
	MaxInstanceLifetime *int32
	// MaxSize is the max number of nodes in asg
	MaxSize *int32
	// Metrics is a collection of metrics to monitor
	Metrics []string
	// MinSize is the smallest number of nodes in the asg
	MinSize *int32
	// MixedInstanceOverrides is a collection of instance types to use with fleet policy
	MixedInstanceOverrides []string
	// InstanceRequirements is a list of requirements for any instance type we are willing to run in the EC2 fleet.
	InstanceRequirements *InstanceRequirements
	// MixedOnDemandAllocationStrategy is allocation strategy to use for on-demand instances
	MixedOnDemandAllocationStrategy *string
	// MixedOnDemandBase is percentage split of On-Demand Instances and Spot Instances for your
	// additional capacity beyond the base portion
	MixedOnDemandBase *int32
	// MixedOnDemandAboveBase is the percentage split of On-Demand Instances and Spot Instances
	// for your additional capacity beyond the base portion.
	MixedOnDemandAboveBase *int32
	// MixedSpotAllocationStrategy diversifies your Spot capacity across multiple instance types to
	// find the best pricing. Higher Spot availability may result from a larger number of
	// instance types to choose from.
	MixedSpotAllocationStrategy *string
	// MixedSpotInstancePools is the number of Spot pools to use to allocate your Spot capacity (defaults to 2)
	// pools are determined from the different instance types in the Overrides array of LaunchTemplate
	MixedSpotInstancePools *int32
	// MixedSpotMaxPrice is the maximum price per unit hour you are willing to pay for a Spot Instance
	MixedSpotMaxPrice *string
	// Subnets is a collection of subnets to attach the nodes to
	Subnets []*Subnet
	// SuspendProcesses
	SuspendProcesses *[]string
	// Tags is a collection of keypairs to apply to the node on launch
	Tags map[string]string
	// TargetGroups is a list of ALB/NLB target group ARNs to add to the autoscaling group
	TargetGroups []*TargetGroup
	// CapacityRebalance makes ASG proactively replace spot instances when ASG receives a rebalance recommendation
	CapacityRebalance *bool

	// WarmPool is the WarmPool config for the ASG.
	// It is marked to be ignored in JSON marshalling to avoid a circular dependency.
	WarmPool *WarmPool `json:"-"`

	deletions []fi.CloudupDeletion
}

var _ fi.CloudupProducesDeletions = &AutoscalingGroup{}
var _ fi.CompareWithID = &AutoscalingGroup{}
var _ fi.CloudupTaskNormalize = &AutoscalingGroup{}

// CompareWithID returns the ID of the ASG
func (e *AutoscalingGroup) CompareWithID() *string {
	return e.Name
}

// Track dependencies here to explicitly ignore WarmPool
// because the WarmPool should be created after the ASG, not the other way around.
// The WarmPool struct field is only used for RenderTerraform.
func (e *AutoscalingGroup) GetDependencies(tasks map[string]fi.CloudupTask) []fi.CloudupTask {
	var deps []fi.CloudupTask

	for _, lb := range e.LoadBalancers {
		deps = append(deps, lb)
	}

	for _, tg := range e.TargetGroups {
		deps = append(deps, tg)
	}

	for _, subnet := range e.Subnets {
		deps = append(deps, subnet)
	}

	if e.LaunchTemplate != nil {
		deps = append(deps, e.LaunchTemplate)
	}

	return deps
}

// Find is used to discover the ASG in the cloud provider
func (e *AutoscalingGroup) Find(c *fi.CloudupContext) (*AutoscalingGroup, error) {
	ctx := c.Context()

	cloud := awsup.GetCloud(c)

	g, err := findAutoscalingGroup(ctx, cloud, fi.ValueOf(e.Name))
	if err != nil {
		return nil, err
	}
	if g == nil {
		return nil, nil
	}

	actual := &AutoscalingGroup{
		Name:                g.AutoScalingGroupName,
		MaxSize:             g.MaxSize,
		MinSize:             g.MinSize,
		MaxInstanceLifetime: g.MaxInstanceLifetime,
	}

	// Use 0 as default value when api returns nil (same as model)
	if g.MaxInstanceLifetime == nil {
		actual.MaxInstanceLifetime = fi.PtrTo(int32(0))
	} else {
		actual.MaxInstanceLifetime = g.MaxInstanceLifetime
	}

	actual.LoadBalancers = []*ClassicLoadBalancer{}
	for _, lb := range g.LoadBalancerNames {
		actual.LoadBalancers = append(actual.LoadBalancers, &ClassicLoadBalancer{
			Name:             aws.String(lb),
			LoadBalancerName: aws.String(lb),
		})
	}

	{
		// pkg/model/awsmodel/autoscalinggroup.go doesn't know the LoadBalancerName of the API ELB task that it passes to the master ASGs,
		// it only knows the LoadBalancerName of external load balancers passed through the InstanceGroupSpec.
		// We lookup the LoadBalancerName for LoadBalancer tasks that don't have it set in order to attach the LB to the ASG.
		//
		// This means some LoadBalancer tasks have LoadBalancerName and others do not.
		// When `Find`ing the ASG and recreating the LoadBalancer tasks we need them to match how the model creates them,
		// but we only know the LoadBalancerNames, not the task names associated with them.
		// This reuslts in spurious changes being reported during subsequent `update cluster` runs because the API ELB task is named differently
		// between the kops model and the ASG's `Find`.
		//
		// To prevent this, we need to update the API ELB task in the ASG's LoadBalancers list.
		// Because we don't know whether any given LoadBalancerName attached to an ASG is the API ELB task or not,
		// we have to find the API ELB task, lookup its LoadBalancerName, and then compare that to the list of attached LoadBalancers.
		var apiLBTask *ClassicLoadBalancer
		for _, lb := range e.LoadBalancers {
			// All external ELBs have their Shared field set to true. The API ELB does not.
			// Note that Shared is set by the kops model rather than AWS tags.
			if !fi.ValueOf(lb.Shared) {
				apiLBTask = lb
			}
		}
		if apiLBTask != nil && len(actual.LoadBalancers) > 0 {
			apiLBDesc, err := awsup.GetCloud(c).FindELBByNameTag(fi.ValueOf(apiLBTask.Name))
			if err != nil {
				return nil, err
			}
			if apiLBDesc != nil {
				for i := 0; i < len(actual.LoadBalancers); i++ {
					lb := actual.LoadBalancers[i]
					if aws.ToString(apiLBDesc.LoadBalancerName) == aws.ToString(lb.Name) {
						actual.LoadBalancers[i] = apiLBTask
					}
				}
			}
		}
	}
	sort.Stable(OrderLoadBalancersByName(actual.LoadBalancers))

	actual.TargetGroups = []*TargetGroup{}
	{
		byARN := make(map[string]*TargetGroup)
		for _, tg := range e.TargetGroups {
			if tg.info != nil {
				byARN[tg.info.ARN] = tg
			}
		}
		for _, arn := range g.TargetGroupARNs {
			tg := byARN[arn]
			if tg != nil {
				actual.TargetGroups = append(actual.TargetGroups, tg)
				continue
			}
			actual.TargetGroups = append(actual.TargetGroups, &TargetGroup{ARN: aws.String(arn)})
			e.deletions = append(e.deletions, buildDeleteAutoscalingTargetGroupAttachment(aws.ToString(g.AutoScalingGroupName), arn))
		}
	}
	sort.Stable(OrderTargetGroupsByName(actual.TargetGroups))

	if g.VPCZoneIdentifier != nil {
		subnets := strings.Split(*g.VPCZoneIdentifier, ",")
		for _, subnet := range subnets {
			actual.Subnets = append(actual.Subnets, &Subnet{ID: aws.String(subnet)})
		}
	}

	for _, enabledMetric := range g.EnabledMetrics {
		actual.Metrics = append(actual.Metrics, aws.ToString(enabledMetric.Metric))
		actual.Granularity = enabledMetric.Granularity
	}
	sort.Strings(actual.Metrics)

	if len(g.Tags) != 0 {
		actual.Tags = make(map[string]string)
		for _, tag := range g.Tags {
			if strings.HasPrefix(aws.ToString(tag.Key), "aws:cloudformation:") {
				continue
			}
			actual.Tags[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
		}
	}

	if g.LaunchTemplate != nil {
		actual.LaunchTemplate = &LaunchTemplate{
			Name: g.LaunchTemplate.LaunchTemplateName,
			ID:   g.LaunchTemplate.LaunchTemplateId,
		}
	}

	if g.MixedInstancesPolicy != nil {
		mp := g.MixedInstancesPolicy
		if mp.InstancesDistribution != nil {
			mpd := mp.InstancesDistribution
			actual.MixedOnDemandAboveBase = mpd.OnDemandPercentageAboveBaseCapacity
			actual.MixedOnDemandAllocationStrategy = mpd.OnDemandAllocationStrategy
			actual.MixedOnDemandBase = mpd.OnDemandBaseCapacity
			actual.MixedSpotAllocationStrategy = mpd.SpotAllocationStrategy
			actual.MixedSpotInstancePools = mpd.SpotInstancePools
			actual.MixedSpotMaxPrice = mpd.SpotMaxPrice
			// MixedSpotMaxPrice must be set to "" in order to unset.
			if mpd.SpotMaxPrice == nil {
				actual.MixedSpotMaxPrice = fi.PtrTo("")
			}
		}

		if g.MixedInstancesPolicy.LaunchTemplate != nil {
			if g.MixedInstancesPolicy.LaunchTemplate.LaunchTemplateSpecification != nil {
				actual.LaunchTemplate = &LaunchTemplate{
					Name: g.MixedInstancesPolicy.LaunchTemplate.LaunchTemplateSpecification.LaunchTemplateName,
					ID:   g.MixedInstancesPolicy.LaunchTemplate.LaunchTemplateSpecification.LaunchTemplateId,
				}
			}

			for _, n := range g.MixedInstancesPolicy.LaunchTemplate.Overrides {
				actual.MixedInstanceOverrides = append(actual.MixedInstanceOverrides, fi.ValueOf(n.InstanceType))
			}
		}
	}

	ir, _ := findInstanceRequirements(g)
	actual.InstanceRequirements = ir

	if subnetSlicesEqualIgnoreOrder(actual.Subnets, e.Subnets) {
		actual.Subnets = e.Subnets
	}

	processes := []string{}
	for _, p := range g.SuspendedProcesses {
		processes = append(processes, *p.ProcessName)
	}

	actual.SuspendProcesses = &processes

	// Avoid spurious changes
	actual.Lifecycle = e.Lifecycle

	if g.NewInstancesProtectedFromScaleIn != nil {
		actual.InstanceProtection = g.NewInstancesProtectedFromScaleIn
	}

	return actual, nil
}

// findAutoscalingGroup is responsible for finding all the autoscaling groups for us
func findAutoscalingGroup(ctx context.Context, cloud awsup.AWSCloud, name string) (*autoscalingtypes.AutoScalingGroup, error) {
	request := &autoscaling.DescribeAutoScalingGroupsInput{
		AutoScalingGroupNames: []string{name},
	}

	var found []*autoscalingtypes.AutoScalingGroup
	paginator := autoscaling.NewDescribeAutoScalingGroupsPaginator(cloud.Autoscaling(), request)
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("error listing AutoScalingGroups: %v", err)
		}
		for _, g := range page.AutoScalingGroups {
			// Check for "Delete in progress" (the only use .Status). We won't be able to update or create while
			// this is true, but filtering it out here makes the messages slightly clearer.
			if g.Status != nil {
				klog.Warningf("Skipping AutoScalingGroup %v: %v", fi.ValueOf(g.AutoScalingGroupName), fi.ValueOf(g.Status))
				continue
			}

			if aws.ToString(g.AutoScalingGroupName) == name {
				found = append(found, &g)
			} else {
				klog.Warningf("Got ASG with unexpected name %q", fi.ValueOf(g.AutoScalingGroupName))
			}
		}
	}

	switch len(found) {
	case 0:
		return nil, nil
	case 1:
		return found[0], nil
	}

	return nil, fmt.Errorf("found multiple AutoscalingGroups with name: %q", name)
}

func (e *AutoscalingGroup) Normalize(c *fi.CloudupContext) error {
	sort.Strings(e.Metrics)
	awsup.GetCloud(c).AddTags(e.Name, e.Tags)

	return nil
}

// Run is responsible for running the task
func (e *AutoscalingGroup) Run(c *fi.CloudupContext) error {
	return fi.CloudupDefaultDeltaRunMethod(e, c)
}

// CheckChanges is responsible for checking for changes??
func (e *AutoscalingGroup) CheckChanges(a, ex, changes *AutoscalingGroup) error {
	if a != nil {
		if ex.Name == nil {
			return fi.RequiredField("Name")
		}
	}

	return nil
}

// RenderAWS is responsible for building the autoscaling group via AWS API
func (v *AutoscalingGroup) RenderAWS(t *awsup.AWSAPITarget, a, e, changes *AutoscalingGroup) error {
	ctx := context.TODO()

	// @step: did we find an autoscaling group?
	if a == nil {
		klog.V(2).Infof("Creating autoscaling group with name: %s", fi.ValueOf(e.Name))

		request := &autoscaling.CreateAutoScalingGroupInput{
			AutoScalingGroupName:             e.Name,
			MinSize:                          e.MinSize,
			MaxSize:                          e.MaxSize,
			NewInstancesProtectedFromScaleIn: e.InstanceProtection,
			Tags:                             v.AutoscalingGroupTags(),
			VPCZoneIdentifier:                fi.PtrTo(strings.Join(e.AutoscalingGroupSubnets(), ",")),
			CapacityRebalance:                e.CapacityRebalance,
		}

		//On ASG creation 0 value is forbidden
		if fi.ValueOf(e.MaxInstanceLifetime) == 0 {
			request.MaxInstanceLifetime = nil
		} else {
			request.MaxInstanceLifetime = e.MaxInstanceLifetime
		}

		for _, k := range e.LoadBalancers {
			if k.LoadBalancerName == nil {
				lbDesc, err := t.Cloud.FindELBByNameTag(fi.ValueOf(k.GetName()))
				if err != nil {
					return err
				}
				if lbDesc == nil {
					return fmt.Errorf("could not find load balancer to attach")
				}
				request.LoadBalancerNames = append(request.LoadBalancerNames, aws.ToString(lbDesc.LoadBalancerName))
			} else {
				request.LoadBalancerNames = append(request.LoadBalancerNames, aws.ToString(k.LoadBalancerName))
			}
		}

		for _, tg := range e.TargetGroups {
			request.TargetGroupARNs = append(request.TargetGroupARNs, aws.ToString(tg.ARN))
		}

		// @check are we using mixed instances policy, or launch template
		if e.UseMixedInstancesPolicy() {
			request.MixedInstancesPolicy = &autoscalingtypes.MixedInstancesPolicy{
				InstancesDistribution: &autoscalingtypes.InstancesDistribution{
					OnDemandPercentageAboveBaseCapacity: e.MixedOnDemandAboveBase,
					OnDemandBaseCapacity:                e.MixedOnDemandBase,
					SpotAllocationStrategy:              e.MixedSpotAllocationStrategy,
					SpotInstancePools:                   e.MixedSpotInstancePools,
					SpotMaxPrice:                        e.MixedSpotMaxPrice,
				},
				LaunchTemplate: &autoscalingtypes.LaunchTemplate{
					LaunchTemplateSpecification: &autoscalingtypes.LaunchTemplateSpecification{
						LaunchTemplateId: e.LaunchTemplate.ID,
						Version:          aws.String("$Latest"),
					},
				},
			}
			p := request.MixedInstancesPolicy.LaunchTemplate
			for _, x := range e.MixedInstanceOverrides {
				p.Overrides = append(p.Overrides, autoscalingtypes.LaunchTemplateOverrides{
					InstanceType: fi.PtrTo(x),
				},
				)
			}
			if e.InstanceRequirements != nil {
				p.Overrides = append(p.Overrides, overridesFromInstanceRequirements(e.InstanceRequirements))
			}
		} else if e.LaunchTemplate != nil {
			request.LaunchTemplate = &autoscalingtypes.LaunchTemplateSpecification{
				LaunchTemplateId: e.LaunchTemplate.ID,
				Version:          aws.String("$Latest"),
			}
		} else {
			return fmt.Errorf("could not find one of launch template or mixed instances policy")
		}

		// @step: attempt to create the autoscaling group for us
		if _, err := t.Cloud.Autoscaling().CreateAutoScalingGroup(ctx, request); err != nil {
			code := awsup.AWSErrorCode(err)
			message := awsup.AWSErrorMessage(err)
			if code == "ValidationError" && strings.Contains(message, "Invalid IAM Instance Profile name") {
				klog.V(4).Infof("error creating AutoscalingGroup: %s", message)
				return fi.NewTryAgainLaterError("waiting for the IAM Instance Profile to be propagated")
			}
			return fmt.Errorf("error creating AutoScalingGroup: %s", message)
		}

		// @step: attempt to enable the metrics for us
		if _, err := t.Cloud.Autoscaling().EnableMetricsCollection(ctx, &autoscaling.EnableMetricsCollectionInput{
			AutoScalingGroupName: e.Name,
			Granularity:          e.Granularity,
			Metrics:              e.Metrics,
		}); err != nil {
			return fmt.Errorf("error enabling metrics collection for AutoscalingGroup: %v", err)
		}

		if len(*e.SuspendProcesses) > 0 {
			processQuery := &autoscaling.SuspendProcessesInput{}
			processQuery.AutoScalingGroupName = e.Name
			processQuery.ScalingProcesses = *e.SuspendProcesses

			if _, err := t.Cloud.Autoscaling().SuspendProcesses(ctx, processQuery); err != nil {
				return fmt.Errorf("error suspending processes: %v", err)
			}
		}

	} else {
		// @logic: else we have found a autoscaling group and we need to evaluate the difference
		request := &autoscaling.UpdateAutoScalingGroupInput{
			AutoScalingGroupName: e.Name,
		}

		setup := func(req *autoscaling.UpdateAutoScalingGroupInput) *autoscalingtypes.MixedInstancesPolicy {
			if req.MixedInstancesPolicy == nil {
				req.MixedInstancesPolicy = &autoscalingtypes.MixedInstancesPolicy{
					InstancesDistribution: &autoscalingtypes.InstancesDistribution{},
				}
			}

			return req.MixedInstancesPolicy
		}

		// We have to update LaunchTemplate to remove mixedInstancesPolicy when it is removed from spec.
		if changes.LaunchTemplate != nil || a.UseMixedInstancesPolicy() && !e.UseMixedInstancesPolicy() {
			spec := &autoscalingtypes.LaunchTemplateSpecification{
				LaunchTemplateId: e.LaunchTemplate.ID,
				Version:          aws.String("$Latest"),
			}
			if e.UseMixedInstancesPolicy() {
				setup(request).LaunchTemplate = &autoscalingtypes.LaunchTemplate{LaunchTemplateSpecification: spec}
			} else {
				request.LaunchTemplate = spec
			}
			changes.LaunchTemplate = nil
		}

		if changes.MixedOnDemandAboveBase != nil {
			setup(request).InstancesDistribution.OnDemandPercentageAboveBaseCapacity = e.MixedOnDemandAboveBase
			changes.MixedOnDemandAboveBase = nil
		}
		if changes.MixedOnDemandBase != nil {
			setup(request).InstancesDistribution.OnDemandBaseCapacity = e.MixedOnDemandBase
			changes.MixedOnDemandBase = nil
		}
		if changes.MixedSpotAllocationStrategy != nil {
			setup(request).InstancesDistribution.SpotAllocationStrategy = e.MixedSpotAllocationStrategy
			changes.MixedSpotAllocationStrategy = nil
		}
		if changes.MixedSpotInstancePools != nil {
			setup(request).InstancesDistribution.SpotInstancePools = e.MixedSpotInstancePools
			changes.MixedSpotInstancePools = nil
		}
		if changes.MixedSpotMaxPrice != nil {
			setup(request).InstancesDistribution.SpotMaxPrice = e.MixedSpotMaxPrice
			changes.MixedSpotMaxPrice = nil
		}
		if changes.MixedInstanceOverrides != nil || changes.InstanceRequirements != nil {
			if setup(request).LaunchTemplate == nil {
				setup(request).LaunchTemplate = &autoscalingtypes.LaunchTemplate{
					LaunchTemplateSpecification: &autoscalingtypes.LaunchTemplateSpecification{
						LaunchTemplateId: e.LaunchTemplate.ID,
						Version:          aws.String("$Latest"),
					},
				}
			}

			if changes.MixedInstanceOverrides != nil {
				p := request.MixedInstancesPolicy.LaunchTemplate
				for _, x := range changes.MixedInstanceOverrides {
					p.Overrides = append(p.Overrides, autoscalingtypes.LaunchTemplateOverrides{InstanceType: fi.PtrTo(x)})
				}
				changes.MixedInstanceOverrides = nil
			}

			if changes.InstanceRequirements != nil {
				p := request.MixedInstancesPolicy.LaunchTemplate

				p.Overrides = append(p.Overrides, overridesFromInstanceRequirements(changes.InstanceRequirements))
				changes.InstanceRequirements = nil
			}
		}

		if changes.MinSize != nil {
			request.MinSize = e.MinSize
			changes.MinSize = nil
		}
		if changes.MaxSize != nil {
			request.MaxSize = e.MaxSize
			changes.MaxSize = nil
		}
		if changes.Subnets != nil {
			request.VPCZoneIdentifier = aws.String(strings.Join(e.AutoscalingGroupSubnets(), ","))
			changes.Subnets = nil
		}

		if changes.MaxInstanceLifetime != nil {
			request.MaxInstanceLifetime = e.MaxInstanceLifetime
			changes.MaxInstanceLifetime = nil
		} else {
			request.MaxInstanceLifetime = fi.PtrTo(int32(0))
		}

		var updateTagsRequest *autoscaling.CreateOrUpdateTagsInput
		var deleteTagsRequest *autoscaling.DeleteTagsInput
		if changes.Tags != nil {
			updateTagsRequest = &autoscaling.CreateOrUpdateTagsInput{Tags: e.AutoscalingGroupTags()}

			if a != nil && len(a.Tags) > 0 {
				deleteTagsRequest = &autoscaling.DeleteTagsInput{}
				deleteTagsRequest.Tags = e.getASGTagsToDelete(a.Tags)
			}

			changes.Tags = nil
		}

		var attachLBRequest *autoscaling.AttachLoadBalancersInput
		var detachLBRequest *autoscaling.DetachLoadBalancersInput
		if changes.LoadBalancers != nil {
			if e != nil && len(e.LoadBalancers) > 0 {
				attachLBRequest = &autoscaling.AttachLoadBalancersInput{
					AutoScalingGroupName: e.Name,
					LoadBalancerNames:    e.AutoscalingLoadBalancers(),
				}
			}

			if a != nil && len(a.LoadBalancers) > 0 {
				detachLBRequest = &autoscaling.DetachLoadBalancersInput{AutoScalingGroupName: e.Name}
				detachLBRequest.LoadBalancerNames = e.getLBsToDetach(a.LoadBalancers)
			}

			changes.LoadBalancers = nil
		}

		var attachTGRequests []*autoscaling.AttachLoadBalancerTargetGroupsInput
		if changes.TargetGroups != nil {
			if e != nil && len(e.TargetGroups) > 0 {
				for _, tgsChunkToAttach := range sliceChunks(e.AutoscalingTargetGroups(), attachLoadBalancerTargetGroupsMaxItems) {
					attachTGRequests = append(attachTGRequests, &autoscaling.AttachLoadBalancerTargetGroupsInput{
						AutoScalingGroupName: e.Name,
						TargetGroupARNs:      tgsChunkToAttach,
					})
				}
			}

			// Detaching is done in a deletion task

			changes.TargetGroups = nil
		}

		if changes.Metrics != nil || changes.Granularity != nil {
			// TODO: Support disabling metrics?
			if len(e.Metrics) != 0 {
				_, err := t.Cloud.Autoscaling().EnableMetricsCollection(ctx, &autoscaling.EnableMetricsCollectionInput{
					AutoScalingGroupName: e.Name,
					Granularity:          e.Granularity,
					Metrics:              e.Metrics,
				})
				if err != nil {
					return fmt.Errorf("error enabling metrics collection for AutoscalingGroup: %v", err)
				}
				changes.Metrics = nil
				changes.Granularity = nil
			}
		}

		if changes.SuspendProcesses != nil {
			toSuspend := processCompare(e.SuspendProcesses, a.SuspendProcesses)
			toResume := processCompare(a.SuspendProcesses, e.SuspendProcesses)

			if len(toSuspend) > 0 {
				suspendProcessQuery := &autoscaling.SuspendProcessesInput{}
				suspendProcessQuery.AutoScalingGroupName = e.Name
				suspendProcessQuery.ScalingProcesses = aws.ToStringSlice(toSuspend)

				_, err := t.Cloud.Autoscaling().SuspendProcesses(ctx, suspendProcessQuery)
				if err != nil {
					return fmt.Errorf("error suspending processes: %v", err)
				}
			}
			if len(toResume) > 0 {
				resumeProcessQuery := &autoscaling.ResumeProcessesInput{}
				resumeProcessQuery.AutoScalingGroupName = e.Name
				resumeProcessQuery.ScalingProcesses = aws.ToStringSlice(toResume)

				_, err := t.Cloud.Autoscaling().ResumeProcesses(ctx, resumeProcessQuery)
				if err != nil {
					return fmt.Errorf("error resuming processes: %v", err)
				}
			}
			changes.SuspendProcesses = nil
		}

		if changes.InstanceProtection != nil {
			request.NewInstancesProtectedFromScaleIn = e.InstanceProtection
			changes.InstanceProtection = nil
		}

		if changes.CapacityRebalance != nil {
			request.CapacityRebalance = e.CapacityRebalance
			changes.CapacityRebalance = nil
		}

		empty := &AutoscalingGroup{}
		if !reflect.DeepEqual(empty, changes) {
			klog.Warningf("cannot apply changes to AutoScalingGroup: %v", changes)
		}

		klog.V(2).Infof("Updating autoscaling group %s", fi.ValueOf(e.Name))

		if _, err := t.Cloud.Autoscaling().UpdateAutoScalingGroup(ctx, request); err != nil {
			return fmt.Errorf("error updating AutoscalingGroup: %v", err)
		}

		if deleteTagsRequest != nil && len(deleteTagsRequest.Tags) > 0 {
			if _, err := t.Cloud.Autoscaling().DeleteTags(ctx, deleteTagsRequest); err != nil {
				return fmt.Errorf("error deleting old AutoscalingGroup tags: %v", err)
			}
		}
		if updateTagsRequest != nil {
			if _, err := t.Cloud.Autoscaling().CreateOrUpdateTags(ctx, updateTagsRequest); err != nil {
				return fmt.Errorf("error updating AutoscalingGroup tags: %v", err)
			}
		}

		if detachLBRequest != nil {
			if _, err := t.Cloud.Autoscaling().DetachLoadBalancers(ctx, detachLBRequest); err != nil {
				return fmt.Errorf("error detatching LoadBalancers: %v", err)
			}
		}
		if attachLBRequest != nil {
			if _, err := t.Cloud.Autoscaling().AttachLoadBalancers(ctx, attachLBRequest); err != nil {
				return fmt.Errorf("error attaching LoadBalancers: %v", err)
			}
		}
		if len(attachTGRequests) > 0 {
			for _, attachTGRequest := range attachTGRequests {
				if _, err := t.Cloud.Autoscaling().AttachLoadBalancerTargetGroups(ctx, attachTGRequest); err != nil {
					return fmt.Errorf("failed to attach target groups: %v", err)
				}
			}
		}
	}

	return nil
}

// UseMixedInstancesPolicy checks if we should add a mixed instances policy to the asg
func (e *AutoscalingGroup) UseMixedInstancesPolicy() bool {
	if e.LaunchTemplate == nil {
		return false
	}
	// @check if any of the mixed instance policies settings are toggled
	if e.MixedOnDemandAboveBase != nil {
		return true
	}
	if e.MixedOnDemandBase != nil {
		return true
	}
	if e.MixedSpotAllocationStrategy != nil {
		return true
	}
	if e.MixedSpotInstancePools != nil {
		return true
	}
	if len(e.MixedInstanceOverrides) > 0 {
		return true
	}
	if e.MixedSpotMaxPrice != nil {
		return true
	}
	if e.InstanceRequirements != nil {
		return true
	}

	return false
}

// AutoscalingGroupTags is responsible for generating the tagging for the asg
func (e *AutoscalingGroup) AutoscalingGroupTags() []autoscalingtypes.Tag {
	var list []autoscalingtypes.Tag

	for k, v := range e.Tags {
		list = append(list, autoscalingtypes.Tag{
			Key:               aws.String(k),
			Value:             aws.String(v),
			ResourceId:        e.Name,
			ResourceType:      aws.String("auto-scaling-group"),
			PropagateAtLaunch: aws.Bool(true),
		})
	}

	return list
}

// AutoscalingGroupSubnets returns the subnets list
func (e *AutoscalingGroup) AutoscalingGroupSubnets() []string {
	var list []string

	for _, x := range e.Subnets {
		list = append(list, fi.ValueOf(x.ID))
	}

	return list
}

// AutoscalingLoadBalancers returns a list of LBs attatched to the ASG
func (e *AutoscalingGroup) AutoscalingLoadBalancers() []string {
	var list []string

	for _, v := range e.LoadBalancers {
		list = append(list, aws.ToString(v.LoadBalancerName))
	}

	return list
}

// AutoscalingTargetGroups returns a list of TGs attatched to the ASG
func (e *AutoscalingGroup) AutoscalingTargetGroups() []string {
	var list []string

	for _, v := range e.TargetGroups {
		list = append(list, aws.ToString(v.ARN))
	}

	return list
}

// processCompare returns processes that exist in a but not in b
func processCompare(a *[]string, b *[]string) []*string {
	notInB := []*string{}
	for _, ap := range *a {
		found := false
		for _, bp := range *b {
			if ap == bp {
				found = true
				break
			}
		}
		if !found {
			notFound := ap
			notInB = append(notInB, &notFound)
		}
	}
	return notInB
}

// getASGTagsToDelete loops through the currently set tags and builds a list of
// tags to be deleted from the Autoscaling Group
func (e *AutoscalingGroup) getASGTagsToDelete(currentTags map[string]string) []autoscalingtypes.Tag {
	tagsToDelete := []autoscalingtypes.Tag{}

	for k, v := range currentTags {
		if _, ok := e.Tags[k]; !ok {
			tagsToDelete = append(tagsToDelete, autoscalingtypes.Tag{
				Key:          aws.String(k),
				Value:        aws.String(v),
				ResourceId:   e.Name,
				ResourceType: aws.String("auto-scaling-group"),
			})
		}
	}
	return tagsToDelete
}

// getLBsToDetach loops through the currently set LBs and builds a list of
// LBs to be detached from the Autoscaling Group
func (e *AutoscalingGroup) getLBsToDetach(currentLBs []*ClassicLoadBalancer) []string {
	lbsToDetach := []string{}
	desiredLBs := map[string]bool{}

	for _, v := range e.LoadBalancers {
		desiredLBs[*v.Name] = true
	}

	for _, v := range currentLBs {
		if _, ok := desiredLBs[*v.Name]; !ok {
			lbsToDetach = append(lbsToDetach, aws.ToString(v.Name))
		}
	}
	return lbsToDetach
}

// getTGsToDetach loops through the currently set LBs and builds a list of
// target groups to be detached from the Autoscaling Group
func (e *AutoscalingGroup) getTGsToDetach(currentTGs []*TargetGroup) []*string {
	tgsToDetach := []*string{}
	desiredTGs := map[string]bool{}

	for _, v := range e.TargetGroups {
		desiredTGs[*v.ARN] = true
	}

	for _, v := range currentTGs {
		if _, ok := desiredTGs[*v.ARN]; !ok {
			tgsToDetach = append(tgsToDetach, v.ARN)
		}
	}
	return tgsToDetach
}

// sliceChunks returns a chunked slice
func sliceChunks(slice []string, chunkSize int) [][]string {
	var chunks [][]string
	for i := 0; i < len(slice); i = i + chunkSize {
		var chunk []string
		if i+chunkSize < len(slice) {
			chunk = slice[i : i+chunkSize]
		} else {
			chunk = slice[i:]
		}
		chunks = append(chunks, chunk)
	}
	return chunks
}

type terraformASGTag struct {
	Key               *string `cty:"key"`
	Value             *string `cty:"value"`
	PropagateAtLaunch *bool   `cty:"propagate_at_launch"`
}

type terraformAutoscalingLaunchTemplateSpecification struct {
	// LaunchTemplateID is the ID of the template to use.
	LaunchTemplateID *terraformWriter.Literal `cty:"id"`
	// Version is the version of the Launch Template to use.
	Version *terraformWriter.Literal `cty:"version"`
}

type terraformAutoscalingMixedInstancesPolicyLaunchTemplateSpecification struct {
	// LaunchTemplateID is the ID of the template to use
	LaunchTemplateID *terraformWriter.Literal `cty:"launch_template_id"`
	// Version is the version of the Launch Template to use
	Version *terraformWriter.Literal `cty:"version"`
}

type terraformAutoscalingMixedInstancesPolicyLaunchTemplateOverride struct {
	// InstanceType is the instance to use
	InstanceType *string `cty:"instance_type"`
}

type terraformAutoscalingMixedInstancesPolicyLaunchTemplate struct {
	// LaunchTemplateSpecification is the definition for a LT
	LaunchTemplateSpecification []*terraformAutoscalingMixedInstancesPolicyLaunchTemplateSpecification `cty:"launch_template_specification"`
	// Override the is machine type override
	Override []*terraformAutoscalingMixedInstancesPolicyLaunchTemplateOverride `cty:"override"`
}

type terraformAutoscalingInstanceDistribution struct {
	// OnDemandAllocationStrategy
	OnDemandAllocationStrategy *string `cty:"on_demand_allocation_strategy"`
	// OnDemandBaseCapacity is the base ondemand requirement
	OnDemandBaseCapacity *int32 `cty:"on_demand_base_capacity"`
	// OnDemandPercentageAboveBaseCapacity is the percentage above base for on-demand instances
	OnDemandPercentageAboveBaseCapacity *int32 `cty:"on_demand_percentage_above_base_capacity"`
	// SpotAllocationStrategy is the spot allocation stratergy
	SpotAllocationStrategy *string `cty:"spot_allocation_strategy"`
	// SpotInstancePool is the number of pools
	SpotInstancePool *int32 `cty:"spot_instance_pools"`
	// SpotMaxPrice is the max bid on spot instance, defaults to demand value
	SpotMaxPrice *string `cty:"spot_max_price"`
}

type terraformMixedInstancesPolicy struct {
	// LaunchTemplate is the launch template spec
	LaunchTemplate []*terraformAutoscalingMixedInstancesPolicyLaunchTemplate `cty:"launch_template"`
	// InstanceDistribution is the distribution strategy
	InstanceDistribution []*terraformAutoscalingInstanceDistribution `cty:"instances_distribution"`
}

type terraformWarmPool struct {
	MinSize *int32 `cty:"min_size"`
	MaxSize *int32 `cty:"max_group_prepared_capacity"`
}

type terraformAutoscalingGroup struct {
	Name                    *string                                          `cty:"name"`
	LaunchConfigurationName *terraformWriter.Literal                         `cty:"launch_configuration"`
	LaunchTemplate          *terraformAutoscalingLaunchTemplateSpecification `cty:"launch_template"`
	MaxSize                 *int32                                           `cty:"max_size"`
	MinSize                 *int32                                           `cty:"min_size"`
	MixedInstancesPolicy    []*terraformMixedInstancesPolicy                 `cty:"mixed_instances_policy"`
	VPCZoneIdentifier       []*terraformWriter.Literal                       `cty:"vpc_zone_identifier"`
	Tags                    []*terraformASGTag                               `cty:"tag"`
	MetricsGranularity      *string                                          `cty:"metrics_granularity"`
	EnabledMetrics          []*string                                        `cty:"enabled_metrics"`
	SuspendedProcesses      []*string                                        `cty:"suspended_processes"`
	InstanceProtection      *bool                                            `cty:"protect_from_scale_in"`
	LoadBalancers           []*terraformWriter.Literal                       `cty:"load_balancers"`
	TargetGroupARNs         []*terraformWriter.Literal                       `cty:"target_group_arns"`
	MaxInstanceLifetime     *int32                                           `cty:"max_instance_lifetime"`
	CapacityRebalance       *bool                                            `cty:"capacity_rebalance"`
	WarmPool                *terraformWarmPool                               `cty:"warm_pool"`
}

// RenderTerraform is responsible for rendering the terraform codebase
func (_ *AutoscalingGroup) RenderTerraform(t *terraform.TerraformTarget, a, e, changes *AutoscalingGroup) error {
	tf := &terraformAutoscalingGroup{
		Name:                e.Name,
		MinSize:             e.MinSize,
		MaxSize:             e.MaxSize,
		MetricsGranularity:  e.Granularity,
		EnabledMetrics:      aws.StringSlice(e.Metrics),
		InstanceProtection:  e.InstanceProtection,
		MaxInstanceLifetime: e.MaxInstanceLifetime,
		CapacityRebalance:   e.CapacityRebalance,
	}

	for _, s := range e.Subnets {
		tf.VPCZoneIdentifier = append(tf.VPCZoneIdentifier, s.TerraformLink())
	}

	for _, k := range maps.SortedKeys(e.Tags) {
		v := e.Tags[k]
		tf.Tags = append(tf.Tags, &terraformASGTag{
			Key:               fi.PtrTo(k),
			Value:             fi.PtrTo(v),
			PropagateAtLaunch: fi.PtrTo(true),
		})
	}

	for _, k := range e.LoadBalancers {
		tf.LoadBalancers = append(tf.LoadBalancers, k.TerraformLink())
	}
	terraformWriter.SortLiterals(tf.LoadBalancers)

	for _, tg := range e.TargetGroups {
		tf.TargetGroupARNs = append(tf.TargetGroupARNs, tg.TerraformLink())
	}
	terraformWriter.SortLiterals(tf.TargetGroupARNs)

	if e.UseMixedInstancesPolicy() {
		// Temporary warning until https://github.com/terraform-providers/terraform-provider-aws/issues/9750 is resolved
		if e.MixedSpotAllocationStrategy == fi.PtrTo("capacity-optimized") {
			fmt.Print("Terraform does not currently support a capacity optimized strategy - please see https://github.com/terraform-providers/terraform-provider-aws/issues/9750")
		}

		tf.MixedInstancesPolicy = []*terraformMixedInstancesPolicy{
			{
				LaunchTemplate: []*terraformAutoscalingMixedInstancesPolicyLaunchTemplate{
					{
						LaunchTemplateSpecification: []*terraformAutoscalingMixedInstancesPolicyLaunchTemplateSpecification{
							{
								LaunchTemplateID: e.LaunchTemplate.TerraformLink(),
								Version:          e.LaunchTemplate.VersionLink(),
							},
						},
					},
				},
				InstanceDistribution: []*terraformAutoscalingInstanceDistribution{
					{
						OnDemandAllocationStrategy:          e.MixedOnDemandAllocationStrategy,
						OnDemandBaseCapacity:                e.MixedOnDemandBase,
						OnDemandPercentageAboveBaseCapacity: e.MixedOnDemandAboveBase,
						SpotAllocationStrategy:              e.MixedSpotAllocationStrategy,
						SpotInstancePool:                    e.MixedSpotInstancePools,
						SpotMaxPrice:                        e.MixedSpotMaxPrice,
					},
				},
			},
		}

		for _, x := range e.MixedInstanceOverrides {
			tf.MixedInstancesPolicy[0].LaunchTemplate[0].Override = append(tf.MixedInstancesPolicy[0].LaunchTemplate[0].Override, &terraformAutoscalingMixedInstancesPolicyLaunchTemplateOverride{InstanceType: fi.PtrTo(x)})
		}
	} else if e.LaunchTemplate != nil {
		tf.LaunchTemplate = &terraformAutoscalingLaunchTemplateSpecification{
			LaunchTemplateID: e.LaunchTemplate.TerraformLink(),
			Version:          e.LaunchTemplate.VersionLink(),
		}
	} else {
		return fmt.Errorf("could not find one of launch configuration, mixed instances policy, or launch template")
	}

	role := ""
	for k := range e.Tags {
		if strings.HasPrefix(k, CloudTagInstanceGroupRolePrefix) {
			suffix := strings.TrimPrefix(k, CloudTagInstanceGroupRolePrefix)
			if suffix == "control-plane" {
				suffix = "master"
			}
			if role != "" && role != suffix {
				return fmt.Errorf("Found multiple role tags: %q vs %q", role, suffix)
			}
			role = suffix
		}
	}

	if e.LaunchTemplate != nil && role != "" {
		for _, sg := range e.LaunchTemplate.SecurityGroups {
			if err := t.AddOutputVariableArray(role+"_security_group_ids", sg.TerraformLink()); err != nil {
				return err
			}
		}
	}
	if role != "" {
		if err := t.AddOutputVariableArray(role+"_autoscaling_group_ids", e.TerraformLink()); err != nil {
			return err
		}
	}
	if role == "node" {
		for _, s := range e.Subnets {
			if err := t.AddOutputVariableArray(role+"_subnet_ids", s.TerraformLink()); err != nil {
				return err
			}
		}
	}

	var processes []*string
	if e.SuspendProcesses != nil {
		for _, p := range *e.SuspendProcesses {
			processes = append(processes, fi.PtrTo(p))
		}
	}
	tf.SuspendedProcesses = processes

	if e.WarmPool != nil && *e.WarmPool.Enabled {
		tf.WarmPool = &terraformWarmPool{
			MinSize: &e.WarmPool.MinSize,
			MaxSize: e.WarmPool.MaxSize,
		}
	}

	return t.RenderResource("aws_autoscaling_group", *e.Name, tf)
}

// TerraformLink fills in the property
func (e *AutoscalingGroup) TerraformLink() *terraformWriter.Literal {
	return terraformWriter.LiteralProperty("aws_autoscaling_group", fi.ValueOf(e.Name), "id")
}

func (e *AutoscalingGroup) FindDeletions(context *fi.CloudupContext) ([]fi.CloudupDeletion, error) {
	return e.deletions, nil
}

type deleteAutoscalingTargetGroupAttachment struct {
	autoScalingGroupName string
	targetGroupARN       string
}

var _ fi.CloudupDeletion = &deleteAutoscalingTargetGroupAttachment{}

func buildDeleteAutoscalingTargetGroupAttachment(autoScalingGroupName string, targetGroupARN string) *deleteAutoscalingTargetGroupAttachment {
	d := &deleteAutoscalingTargetGroupAttachment{}
	d.autoScalingGroupName = autoScalingGroupName
	d.targetGroupARN = targetGroupARN
	return d
}

func (d *deleteAutoscalingTargetGroupAttachment) Delete(t fi.CloudupTarget) error {
	ctx := context.TODO()

	awsTarget, ok := t.(*awsup.AWSAPITarget)
	if !ok {
		return fmt.Errorf("unexpected target type for deletion: %T", t)
	}

	req := &autoscaling.DetachLoadBalancerTargetGroupsInput{
		AutoScalingGroupName: aws.String(d.autoScalingGroupName),
		TargetGroupARNs:      []string{d.targetGroupARN},
	}
	if _, err := awsTarget.Cloud.Autoscaling().DetachLoadBalancerTargetGroups(ctx, req); err != nil {
		return fmt.Errorf("failed to detach target groups from autoscaling group: %v", err)
	}

	return nil
}

func (d *deleteAutoscalingTargetGroupAttachment) TaskName() string {
	return "autoscaling-elb-attachment"

}
func (d *deleteAutoscalingTargetGroupAttachment) Item() string {
	return d.autoScalingGroupName + ":" + d.targetGroupARN
}

func (d *deleteAutoscalingTargetGroupAttachment) DeferDeletion() bool {
	return true
}
