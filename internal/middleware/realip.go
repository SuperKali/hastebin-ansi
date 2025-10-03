package middleware

import (
	"net/http"
	"strings"
)

func RealIP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cfIP := r.Header.Get("CF-Connecting-IP"); cfIP != "" {
			r.RemoteAddr = cfIP
		} else if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			ips := strings.Split(xff, ",")
			if len(ips) > 0 {
				r.RemoteAddr = strings.TrimSpace(ips[0])
			}
		} else if xri := r.Header.Get("X-Real-IP"); xri != "" {
			r.RemoteAddr = xri
		}
		next.ServeHTTP(w, r)
	})
}
