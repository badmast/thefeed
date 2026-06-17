package web

import (
	"encoding/base64"
	"net/http"
	"strings"
)

// Built-in starter profiles offered in the import UI. Both share the
// resolver preset below.

type defaultProfile struct {
	Nickname     string   `json:"nickname"`
	Domain       string   `json:"domain"`
	Key          string   `json:"key"`
	ServerKey    string   `json:"serverKey"`              // pinned server signing pubkey (base64url); empty = unverified
	ExtraDomains []string `json:"extraDomains,omitempty"` // extra sub-domains feed queries spread across
}

// Domain, key and extra domains are stored base64-encoded so the plain values
// are not indexed by code search. ServerKey is the server's signing public key
// (base64url, from `thefeed-server -print-pubkey`) and clients verify feed
// content against it.
var defaultProfiles = []defaultProfile{
	{
		Nickname:  "اخبار و تحلیل",
		Domain:    b64("bndzLnRoZWZlZWQud2Vic2l0ZQ=="),
		Key:       b64("c2FydG8="),
		ServerKey: "bsxz20Z9FWr1h24ATWHoo9cKwfphLpgLoJDhr-KT_f8",
		ExtraDomains: []string{
			b64("bndzLnRoZWZlZWQuYXNpYQ=="),
			b64("d3MxLmZlZWR0aGVmZWVkLnN0b3Jl"),
			b64("d3MxLnRoZWZlZWQuYXNpYQ=="),
			b64("bndzLmZlZWR0aGVmZWVkLnN0b3Jl"),
			b64("d3MxLnRoZWZlZWQud2Vic2l0ZQ=="),
		},
	},
	{
		Nickname:  "فیلترشکن",
		Domain:    b64("Y2ZnLnRoZWZlZWQud2Vic2l0ZQ=="),
		Key:       b64("c2FydG8="),
		ServerKey: "z9gH9ZobPhMZFhB7n3aAX3Xvp3J4FZSeHn54DWQPFhE",
		ExtraDomains: []string{
			b64("Y2YyLnRoZWZlZWQud2Vic2l0ZQ=="),
			b64("Y2ZnLnRoZWZlZWQuYXNpYQ=="),
			b64("Y2YyLnRoZWZlZWQuYXNpYQ=="),
			b64("Y2ZnLmZlZWR0aGVmZWVkLnN0b3Jl"),
			b64("Y2cyLmZlZWR0aGVmZWVkLnN0b3Jl"),
		},
	},
	{
		Nickname:  "توییتر",
		Domain:    b64("dC5oYXBweWxldHMud2lu"),
		Key:       b64("SXJhbmVBemFk"),
		ServerKey: "P13ug6RkRVySwdPJY-ba3JYms7HoeWj49YkTLgANG8w",
	},
}

// b64 decodes a base64 literal; returns "" on malformed input.
func b64(s string) string {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return ""
	}
	return string(b)
}

// handleProfileDefaults serves the built-in starter profiles.
// GET → {profiles: [{nickname, domain, key}], resolvers: [...]}.
func (s *Server) handleProfileDefaults(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	writeJSON(w, map[string]any{
		"profiles":  defaultProfiles,
		"resolvers": parseDefaultProfileResolvers(),
	})
}

// parseDefaultProfileResolvers returns the non-empty, non-comment lines
// from the preset.
func parseDefaultProfileResolvers() []string {
	lines := strings.Split(defaultProfileResolvers, "\n")
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		out = append(out, l)
	}
	return out
}

// defaultProfileResolvers is the shared resolver preset for the
// built-in profiles.
const defaultProfileResolvers = `
1.1.1.1:53
185.109.61.27:53
78.111.11.12:53
217.66.203.211:53
8.8.8.8:53
78.111.11.11:53
46.100.55.232:53
178.252.178.205:53
185.208.76.103:53
85.185.1.10:53
62.60.197.83:53
80.191.202.15:53
77.238.123.238:53
193.186.32.32:53
185.125.244.3:53
5.22.203.102:53
78.38.0.82:53
78.157.41.60:53
217.144.107.162:53
185.208.76.106:53
89.46.61.219:53
81.91.139.18:53
178.131.180.73:53
109.125.136.149:53
31.25.134.99:53
78.38.26.132:53
109.125.169.20:53
185.137.25.210:53
78.38.26.124:53
78.38.26.135:53
89.144.166.195:53
217.219.34.66:53
89.144.189.135:53
10.104.204.79:53
94.182.17.205:53
93.118.128.38:53
80.191.60.36:53
10.104.204.183:53
188.75.65.221:53
185.42.225.197:53
91.92.187.10:53
37.255.206.180:53
2.187.33.174:53
80.210.58.59:53
78.39.80.9:53
81.16.121.93:53
5.160.1.42:53
5.190.8.113:53
82.99.230.170:53
2.181.234.140:53
91.106.67.85:53
93.118.125.5:53
5.202.171.82:53
94.182.192.170:53
217.218.127.127:53
93.118.164.193:53
80.210.47.149:53
94.183.127.163:53
94.183.24.50:53
93.118.148.214:53
5.202.37.55:53
109.125.140.186:53
78.39.117.48:53
2.180.6.146:53
2.187.249.178:53
78.38.26.130:53
217.219.196.226:53
46.100.49.96:53
81.163.0.138:53
92.246.144.205:53
10.104.209.71:53
109.230.72.243:53
84.241.3.105:53
81.16.112.6:53
5.202.97.22:53
2.186.118.255:53
185.129.236.194:53
194.147.167.221:53
185.117.139.84:53
94.183.28.24:53
80.210.40.54:53
109.230.78.13:53
91.92.190.84:53
2.182.253.245:53
217.218.155.155:53
10.104.204.55:53
10.104.204.63:53
10.104.204.247:53
87.107.9.173:53
10.104.204.207:53
10.104.209.247:53
77.104.115.239:53
185.14.161.36:53
185.255.89.57:53
109.230.79.12:53
109.95.61.243:53
176.65.240.86:53
2.186.117.93:53
82.99.247.45:53
185.112.36.134:53
93.118.123.9:53
185.208.148.211:53
185.231.181.206:53
185.24.255.148:53
185.24.255.80:53
188.121.96.94:53
185.112.149.118:53
217.219.66.8:53
10.104.33.246:53
80.210.54.182:53
94.182.17.206:53
89.46.219.198:53
91.222.196.8:53
109.230.223.75:53
37.191.79.105:53
5.202.78.2:53
109.109.32.10:53
80.210.44.187:53
78.157.52.10:53
80.210.53.97:53
109.109.32.125:53
109.109.32.102:53
109.109.32.110:53
109.109.32.21:53
2.180.31.174:53
89.46.219.84:53
93.115.122.89:53
93.118.131.12:53
217.219.120.82:53
81.16.124.73:53
37.202.186.29:53
46.209.48.5:53
31.47.32.34:53
46.100.90.168:53
194.53.122.168:53
93.118.115.240:53
194.53.122.139:53
2.189.44.68:53
2.189.44.98:53
51.38.215.195:53
2.189.44.92:53
2.189.44.91:53
2.189.44.90:53
2.189.44.93:53
2.189.44.94:53
2.189.44.85:53
2.189.44.82:53
2.189.44.83:53
2.189.44.86:53
2.189.44.84:53
2.188.21.138:53
2.189.44.14:53
194.61.120.143:53
151.232.36.4:53
162.158.209.131:53
193.178.200.3:53
2.144.6.75:53
46.100.14.49:53
94.184.10.132:53
89.40.78.92:53
2.144.198.247:53
94.184.10.135:53
5.236.104.215:53
85.185.163.4:53
185.208.183.29:53
87.107.9.233:53
87.107.9.170:53
94.74.128.185:53
10.104.204.215:53
178.252.129.108:53
31.7.78.205:53
185.121.129.66:53
80.75.14.102:53
193.186.32.141:53
185.224.179.176:53
2.189.101.0:53
46.100.132.34:53
217.218.249.240:53
217.219.141.170:53
84.241.53.153:53
78.157.42.100:53
46.245.76.162:53
185.88.178.196:53
91.92.214.110:53
185.53.142.174:53
217.219.162.200:53
31.214.169.244:53
2.188.21.58:53
2.188.21.70:53
10.224.254.58:53
185.128.82.2:53
2.188.21.46:53
5.160.125.234:53
108.162.192.0:53
156.146.33.97:53
79.127.211.213:53
78.38.77.2:53
172.64.32.0:53
2.188.21.62:53
74.63.24.211:53
173.245.58.0:53
5.160.140.16:53
74.80.77.245:53
217.219.29.66:53
5.56.132.97:53
178.252.143.130:53
207.211.215.145:53
162.159.38.0:53
2.188.21.146:53
185.236.90.12:53
74.63.24.205:53
77.238.123.179:53
79.127.170.12:53
79.127.170.15:53
149.102.250.14:53
94.184.10.131:53
2.189.44.66:53
`
