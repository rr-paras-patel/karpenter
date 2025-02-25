
---
title: "Getting Started with Karpenter on AWS"
linkTitle: "Getting Started"
weight: 10
menu:
  main:
    weight: 10
---

Karpenter automatically provisions new nodes in response to unschedulable
pods. Karpenter does this by observing events within the Kubernetes cluster,
and then sending commands to the underlying cloud provider.

In this example, the cluster is running on Amazon Web Services (AWS) Elastic
Kubernetes Service (EKS). Karpenter is designed to be cloud provider agnostic,
but currently only supports AWS. Contributions are welcomed.

This guide should take less than 1 hour to complete, and cost less than $0.25.
Follow the clean-up instructions to reduce any charges.

## Install

Karpenter is installed in clusters with a helm chart.

Karpenter additionally requires IAM Roles for Service Accounts (IRSA). IRSA
permits Karpenter (within the cluster) to make privileged requests to AWS (as
the cloud provider).

### Required Utilities

Install these tools before proceeding:

1. [AWS CLI](https://docs.aws.amazon.com/cli/latest/userguide/install-cliv2-linux.html)
2. `kubectl` - [the kubernetes CLI](https://kubernetes.io/docs/tasks/tools/install-kubectl-linux/)
3. `eksctl` - [the CLI for AWS EKS](https://docs.aws.amazon.com/eks/latest/userguide/eksctl.html)

Login to the AWS CLI with a user that has sufficient privileges to create a
cluster.

### Environment Variables

After setting up the tools, set the following environment variables to store
commonly used values.

```bash
export CLUSTER_NAME=$USER-karpenter-demo
export AWS_DEFAULT_REGION=us-west-2
AWS_ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
```

### Create a Cluster

Create a cluster with `eksctl`. The [example configuration](eksctl.yaml) file specifies a basic cluster (name, region), and an IAM role for Karpenter to use.

```bash
curl -fsSL https://karpenter.sh/docs/getting-started/eksctl.yaml \
  | envsubst \
  | eksctl create cluster -f -
```

This guide uses a self-managed node group to host Karpenter.

Karpenter itself can run anywhere, including on [self-managed node groups](https://docs.aws.amazon.com/eks/latest/userguide/worker.html), [managed node groups](https://docs.aws.amazon.com/eks/latest/userguide/managed-node-groups.html), or [AWS Fargate](https://aws.amazon.com/fargate/).

Karpenter will provision EC2 instances in your account.

Additionally, the configuration file sets up [IAM Roles for Service Accounts](https://docs.aws.amazon.com/emr/latest/EMR-on-EKS-DevelopmentGuide/setting-up-enable-IAM.html) (IRSA), which grants Karpenter permissions like launching instances.

### Tag Subnets

Karpenter discovers subnets tagged `kubernetes.io/cluster/$CLUSTER_NAME`. Add this tag to subnets associated configured for your cluster.
Retreive the subnet IDs and tag them with the cluster name.

```bash
SUBNET_IDS=$(aws cloudformation describe-stacks \
    --stack-name eksctl-${CLUSTER_NAME}-cluster \
    --query 'Stacks[].Outputs[?OutputKey==`SubnetsPrivate`].OutputValue' \
    --output text)
aws ec2 create-tags \
    --resources $(echo $SUBNET_IDS | tr ',' '\n') \
    --tags Key="kubernetes.io/cluster/${CLUSTER_NAME}",Value=
```

### Setup an IAM InstanceProfile for your Nodes

Instances launched by Karpenter must run with an InstanceProfile that grants permissions necessary to run containers and configure networking. Karpenter discovers the InstanceProfile using the name `KarpenterNodeRole-${ClusterName}`.

First, create the IAM resources using AWS CloudFormation.

```bash
TEMPOUT=$(mktemp)
curl -fsSL https://karpenter.sh/docs/getting-started/cloudformation.yaml > $TEMPOUT \
&& aws cloudformation deploy \
  --stack-name Karpenter-${CLUSTER_NAME} \
  --template-file ${TEMPOUT} \
  --capabilities CAPABILITY_NAMED_IAM \
  --parameter-overrides ClusterName=${CLUSTER_NAME}
```

Second, grant access to instances using the profile to connect to the cluster. This command adds the Karpenter node role to your aws-auth configmap, allowing nodes with this role to connect to the cluster.

```bash
eksctl create iamidentitymapping \
  --username system:node:{{EC2PrivateDNSName}} \
  --cluster  ${CLUSTER_NAME} \
  --arn arn:aws:iam::${AWS_ACCOUNT_ID}:role/KarpenterNodeRole-${CLUSTER_NAME} \
  --group system:bootstrappers \
  --group system:nodes
```

Now, Karpenter can launch new EC2 instances and those instances can connect to your cluster.

### Install Karpenter Helm Chart

Use helm to deploy Karpenter to the cluster.

We created a Kubernetes service account when we created the cluster using
eksctl. Thus, we don't need the helm chart to do that.

```bash
helm repo add karpenter https://awslabs.github.io/karpenter/charts
helm repo update
helm upgrade --install karpenter karpenter/karpenter --namespace karpenter \
  --create-namespace --set serviceAccount.create=false --version 0.2.9
```

### Provisioner

A single Karpenter provisioner is capable of handling many different pod
shapes. Karpenter makes scheduling and provisioning decisions based on pod
attributes such as labels and affinity. In other words, Karpenter eliminates
the need to manage many different node groups.

Create a default provisioner using the command below. This provisioner
configures instances to connect to your cluster's endpoint and discovers
resources like subnets and security groups using the cluster's name.

The `ttlSecondsAfterEmpty` value configures Karpenter to terminate empty nodes.
This behavior can be disabled by leaving the value undefined.

Review the [provsioner CRD](/docs/provisioner-crd) for more information. For example,
`ttlSecondsUntilExpired` configures Karpenter to terminate nodes when a maximum age is reached.

```bash
cat <<EOF | kubectl apply -f -
apiVersion: karpenter.sh/v1alpha3
kind: Provisioner
metadata:
  name: default
spec:
  cluster:
    name: ${CLUSTER_NAME}
    endpoint: $(aws eks describe-cluster --name ${CLUSTER_NAME} --query "cluster.endpoint" --output json)
  ttlSecondsAfterEmpty: 30
EOF
```

## First Use

Karpenter is now active and ready to begin provisioning nodes.
Create some pods using a deployment, and watch Karpenter provision nodes in response.

### Automatic Node Provisioning

This deployment uses the [pause image](https://www.ianlewis.org/en/almighty-pause-container) and starts with zero replicas.

```bash
cat <<EOF | kubectl apply -f -
apiVersion: apps/v1
kind: Deployment
metadata:
  name: inflate
spec:
  replicas: 0
  selector:
    matchLabels:
      app: inflate
  template:
    metadata:
      labels:
        app: inflate
    spec:
      containers:
        - name: inflate
          image: public.ecr.aws/eks-distro/kubernetes/pause:3.2
          resources:
            requests:
              cpu: 1
EOF
kubectl scale deployment inflate --replicas 5
kubectl logs -f -n karpenter $(kubectl get pods -n karpenter -l karpenter=controller -o name)
```

### Automatic Node Termination

Now, delete the deployment. After 30 seconds (`ttlSecondsAfterEmpty`),
Karpenter should terminate the now empty nodes.

```bash
kubectl delete deployment inflate
kubectl logs -f -n karpenter $(kubectl get pods -n karpenter -l karpenter=controller -o name)
```

### Manual Node Termination

If you delete a node with kubectl, Karpenter will gracefully cordon, drain,
and shutdown the corresponding instance. Under the hood, Karpenter adds a
finalizer to the node object, which blocks deletion until all pods are
drained and the instance is terminated. Keep in mind, this only works for
nodes provisioned by Karpenter.

```bash
kubectl delete node $NODE_NAME
```

## Cleanup

To avoid additional charges, remove the demo infrastructure from your AWS account.

```bash
aws cloudformation delete-stack --stack-name Karpenter-${CLUSTER_NAME}
aws ec2 describe-launch-templates \
    | jq -r ".LaunchTemplates[].LaunchTemplateName" \
    | grep -i Karpenter-${CLUSTER_NAME} \
    | xargs -I{} aws ec2 delete-launch-template --launch-template-name {}
eksctl delete cluster --name ${CLUSTER_NAME}
```
