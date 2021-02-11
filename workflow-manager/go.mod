module github.com/letsencrypt/prio-server/workflow-manager

go 1.15

require (
	cloud.google.com/go/pubsub v1.3.1
	cloud.google.com/go/storage v1.12.0
	github.com/aws/aws-sdk-go v1.37.8
	github.com/prometheus/client_golang v1.8.0
	golang.org/x/text v0.3.4 // indirect
	google.golang.org/api v0.33.0
	gopkg.in/retry.v1 v1.0.2
	k8s.io/api v0.19.3
	k8s.io/apimachinery v0.19.3
	k8s.io/client-go v0.19.3
	k8s.io/utils v0.0.0-20201015054608-420da100c033 // indirect
)
