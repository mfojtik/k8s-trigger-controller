package controller

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	runtimeutil "k8s.io/apimachinery/pkg/util/runtime"
)

// calculateDataHash calculates a hash from the map[string]string
// TODO: This might be inefficient, there should be a better way to get a checksum for the current data.
func calculateDataHash(obj interface{}) string {
	hash := sha1.New()
	switch t := obj.(type) {
	case *corev1.Secret:
		var (
			sortedDataKeys       []string
			sortedStringDataKeys []string
		)
		for k := range t.Data {
			sortedDataKeys = append(sortedDataKeys, k)
		}
		for k := range t.StringData {
			sortedStringDataKeys = append(sortedStringDataKeys, k)
		}
		if len(sortedDataKeys) == 0 && len(sortedStringDataKeys) == 0 {
			return ""
		}
		sort.Strings(sortedDataKeys)
		hash.Write([]byte(strings.Join(sortedDataKeys, "")))
		sort.Strings(sortedStringDataKeys)
		hash.Write([]byte(strings.Join(sortedStringDataKeys, "")))
		for _, key := range sortedDataKeys {
			hash.Write(t.Data[key])
		}
		for _, key := range sortedStringDataKeys {
			hash.Write([]byte(t.StringData[key]))
		}
	case *corev1.ConfigMap:
		var (
			sortedDataKeys       []string
			sortedBinaryDataKeys []string
		)
		for k := range t.Data {
			sortedDataKeys = append(sortedDataKeys, k)
		}
		for k := range t.BinaryData {
			sortedBinaryDataKeys = append(sortedBinaryDataKeys, k)
		}
		if len(sortedDataKeys) == 0 && len(sortedBinaryDataKeys) == 0 {
			return ""
		}
		sort.Strings(sortedDataKeys)
		hash.Write([]byte(strings.Join(sortedDataKeys, "")))
		sort.Strings(sortedBinaryDataKeys)
		hash.Write([]byte(strings.Join(sortedBinaryDataKeys, "")))
		for _, key := range sortedDataKeys {
			hash.Write([]byte(t.Data[key]))
		}
		for _, key := range sortedBinaryDataKeys {
			hash.Write([]byte(t.BinaryData[key]))
		}
	default:
		runtimeutil.HandleError(fmt.Errorf("unknown object: %v", obj))
		return ""
	}
	return hex.EncodeToString(hash.Sum(nil))
}
