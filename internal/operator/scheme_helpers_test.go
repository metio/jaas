/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"k8s.io/apimachinery/pkg/runtime/schema"

	jaasv1 "github.com/metio/jaas/api/v1"
)

// seenOptsGV returns the JaaS v1 GroupVersion as a runtime/schema value, used
// by manager_test to assert the scheme handed to the manager carries our types.
func seenOptsGV() schema.GroupVersion {
	return jaasv1.GroupVersion
}
