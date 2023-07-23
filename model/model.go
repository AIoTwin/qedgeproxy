package model

import (
	"time"
)

type PodInfo struct {
	Namespace string
	Name      string
	IP        string
	HostIP    string
}

type NodeMetrics struct {
	CpuUsage float64
	RamUsage float64
}

type HostData struct {
	Latency          int
	IsApproximated   bool
	IsServiceHealthy bool
	ReqTime          time.Time
	FailedReqCounter int
}

type PingCache struct {
	CacheTime time.Time
	Latency   int
}

type PodInfoCache struct {
	CacheTime   time.Time
	Pods        []*PodInfo
	Annotations map[string]string
	TargetPort  string
}
