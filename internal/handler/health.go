/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package handler

import "net/http"

func HealthHandler() http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		writer.WriteHeader(http.StatusOK)
		writer.Header().Set("Content-Type", "application/json")
		_, err := writer.Write([]byte(`{"status": "ok"}`))
		if err != nil {
			return
		}
	}
}
