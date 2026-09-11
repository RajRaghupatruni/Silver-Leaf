package decision

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"

	optiscalev1alpha1 "github.com/RajRaghupatruni/Silver-Leaf/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type Input struct {
	Action          string
	Reason          string
	ObservedMetric  *float64
	Threshold       *float64
	CurrentReplicas int32
	DesiredReplicas int32
	ObservedAt      time.Time
}

func NewRecord(in Input) *optiscalev1alpha1.DecisionRecord {
	return &optiscalev1alpha1.DecisionRecord{
		ID:              ID(in),
		Timestamp:       metav1.NewTime(time.Now().UTC()),
		Action:          optiscalev1alpha1.Action(in.Action),
		Reason:          in.Reason,
		ObservedMetric:  in.ObservedMetric,
		Threshold:       in.Threshold,
		CurrentReplicas: in.CurrentReplicas,
		DesiredReplicas: in.DesiredReplicas,
	}
}

func ID(in Input) string {
	value := fmt.Sprintf("%s|%s|%v|%v|%d|%d|%s", in.Action, in.Reason, pointerValue(in.ObservedMetric), pointerValue(in.Threshold), in.CurrentReplicas, in.DesiredReplicas, in.ObservedAt.UTC().Format(time.RFC3339Nano))
	hash := sha256.Sum256([]byte(value))
	return "dec-" + hex.EncodeToString(hash[:8]) + "-" + strconv.FormatInt(in.ObservedAt.Unix(), 10)
}

func pointerValue(value *float64) string {
	if value == nil {
		return ""
	}
	return strconv.FormatFloat(*value, 'g', -1, 64)
}
