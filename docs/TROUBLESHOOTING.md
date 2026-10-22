# Roxxy Troubleshooting

## CA Certificate Installation Issues

### Problem: Python tools (pip/uv) fail with TLS certificate errors even after installing CA cert

**Symptoms:**
- `update-ca-certificates` runs successfully
- CA certificate appears in `/etc/ssl/certs/` as a symlink
- But Python tools like `pip` or `uv` still fail with certificate verification errors

**Root Cause:**
The `update-ca-certificates` command creates symlinks in `/etc/ssl/certs/` but **does not update the bundle file** (`/etc/ssl/certs/ca-certificates.crt`) that Python tools use. Python's `certifi` package and tools like `pip`/`uv` look for certificates in a bundle file, not individual certificate files.

**Solution:**
After running `update-ca-certificates`, manually append your CA certificate to the bundle file:

```bash
# Install CA cert
cp /path/to/ca-cert.pem /usr/local/share/ca-certificates/roxxy-ca.crt
chmod 644 /usr/local/share/ca-certificates/roxxy-ca.crt
update-ca-certificates

# Manually append to bundle file (required for Python tools)
cat /usr/local/share/ca-certificates/roxxy-ca.crt >> /etc/ssl/certs/ca-certificates.crt

# Set environment variables so Python tools use the bundle
export SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt
export REQUESTS_CA_BUNDLE=/etc/ssl/certs/ca-certificates.crt
export CURL_CA_BUNDLE=/etc/ssl/certs/ca-certificates.crt
```

**Why this is necessary:**
- `update-ca-certificates` creates symlinks: `/etc/ssl/certs/roxxy-ca.pem -> /usr/local/share/ca-certificates/roxxy-ca.crt`
- OpenSSL and system tools can use these symlinks
- But Python's `certifi` package bundles its own CA certificates and doesn't automatically use the system bundle
- Setting `REQUESTS_CA_BUNDLE` tells Python's `requests` library (used by pip/uv) to use the system bundle file instead of certifi's bundle

**Verification:**
```bash
# Check cert is in bundle
grep -q "Roxxy" /etc/ssl/certs/ca-certificates.crt && echo "Cert found in bundle"

# Test Python can use it
python3 -c "import ssl; ctx = ssl.create_default_context(); print('SSL context created successfully')"
```

## NPM Certificate Issues

### Problem: npm fails with `UNABLE_TO_VERIFY_LEAF_SIGNATURE`

**Symptoms:**
- npm install fails with certificate verification errors
- Error: `UNABLE_TO_VERIFY_LEAF_SIGNATURE` or `unable to verify the first certificate`

**Solution:**
npm needs to be explicitly told where to find the CA certificate:

```bash
# Install CA cert (as above)
update-ca-certificates

# Configure npm
npm config set registry https://registry.npmjs.org
npm config set strict-ssl true  # Use true with cafile for proper validation
npm config set cafile /etc/ssl/certs/ca-certificates.crt
```

**Note:** Use `strict-ssl true` (not `false`) when you have a proper CA certificate. Setting it to `false` disables all certificate validation, which is insecure.

## Go Modules Certificate Issues

### Problem: `go get` fails with certificate errors

**Solution:**
Go uses the system trust store by default, so if you've properly installed the CA certificate, it should work. Verify:

```bash
# Check cert is in system bundle
grep -q "Roxxy" /etc/ssl/certs/ca-certificates.crt

# Configure Go proxy
go env -w GOPROXY=https://proxy.golang.org,direct
go env -w GOSUMDB=sum.golang.org
```

If issues persist, you can explicitly set the certificate bundle:

```bash
export SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt
```

## General Certificate Installation Best Practices

1. **Always verify the cert was added:**
   ```bash
   grep -q "Your CA Name" /etc/ssl/certs/ca-certificates.crt && echo "Found" || echo "Not found"
   ```

2. **For Debian/Ubuntu:**
   ```bash
   cp ca-cert.pem /usr/local/share/ca-certificates/roxxy-ca.crt
   chmod 644 /usr/local/share/ca-certificates/roxxy-ca.crt
   update-ca-certificates
   # Then append to bundle for Python tools
   cat /usr/local/share/ca-certificates/roxxy-ca.crt >> /etc/ssl/certs/ca-certificates.crt
   ```

3. **For Alpine:**
   ```bash
   cp ca-cert.pem /usr/local/share/ca-certificates/roxxy-ca.crt
   cat /usr/local/share/ca-certificates/roxxy-ca.crt >> /etc/ssl/certs/ca-certificates.crt
   ```

4. **Set environment variables for Python tools:**
   ```bash
   export SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt
   export REQUESTS_CA_BUNDLE=/etc/ssl/certs/ca-certificates.crt
   export CURL_CA_BUNDLE=/etc/ssl/certs/ca-certificates.crt
   ```

## Proxy Connection Issues

### Problem: Package manager requests timeout or hang

**Check:**
1. Verify proxy is running: `docker ps | grep roxxy` or `ps aux | grep roxxy`
2. Check proxy logs: `docker logs roxxy --tail 50` or `roxxy logs`
3. Verify network connectivity: `curl -v --proxy http://localhost:3128 https://registry.npmjs.org`

### Problem: Proxy loop (proxy trying to proxy its own requests)

**Symptoms:**
- Requests hang indefinitely
- Proxy logs show repeated connection attempts

**Solution:**
Roxxy automatically prevents proxy loops by checking if the configured upstream proxy points to its own listen address. However, if you need to explicitly bypass proxy:

**Via Config:**
```yaml
global:
  upstream_proxy: "none"  # Explicitly bypass proxy
```

**Via Environment:**
```bash
# Unset HTTP_PROXY for roxxy itself
unset HTTP_PROXY
unset HTTPS_PROXY
roxxy server
```

**In Code:**
Roxxy uses `config.GetUpstreamProxyFunc()` which automatically prevents loops. If you're extending roxxy, use this function instead of hardcoding `Proxy: nil`.

## Debugging Tips

1. **Enable verbose logging:**
   - Check roxxy logs: `docker logs roxxy --tail 100` or `roxxy logs`
   - Look for `[npm]`, `[go]`, `[pypi]` prefixes to see which package manager is being used

2. **Test certificate trust:**
   ```bash
   openssl s_client -connect localhost:443 -servername registry.npmjs.org \
     -CAfile /etc/ssl/certs/ca-certificates.crt
   ```

3. **Verify environment variables:**
   ```bash
   env | grep -E "(SSL_CERT|REQUESTS_CA|CURL_CA|HTTP_PROXY|HTTPS_PROXY)"
   ```

4. **Test individual package managers:**
   ```bash
   # NPM
   npm view express version
   
   # PyPI
   pip search requests  # or use uv
   
   # Go
   go get -v github.com/gin-gonic/gin
   ```

## HTTP/2 Protocol Support

**Status:** Roxxy supports both HTTP/2 and HTTP/1.1 for package manager proxies. However, in practice, we have not yet observed clients negotiating HTTP/2 (`h2`) in the wild - most clients appear to use HTTP/1.1.

**How It Works:**
- The proxy advertises both `h2` and `http/1.1` in the TLS `NextProtos` during the handshake
- After the TLS handshake, the proxy checks which protocol was negotiated
- If `h2` is negotiated, it uses `golang.org/x/net/http2.Server` to handle HTTP/2 frames
- If `http/1.1` (or empty, which means HTTP/1.1), it uses manual request handling

**What to Check in Logs:**

When a CONNECT request is handled for a package manager, you should see a log line like:
```
[172.18.0.5:57904] TLS handshake complete for registry.npmjs.org, negotiated protocol: http/1.1
```

**If HTTP/2 is being used, you would see:**
```
[172.18.0.5:57904] TLS handshake complete for registry.npmjs.org, negotiated protocol: h2
```

**Troubleshooting HTTP/2 Issues:**

If you suspect HTTP/2 is causing problems:

1. **Check the negotiated protocol in logs:**
   ```bash
   docker logs roxxy | grep "negotiated protocol"  # or: roxxy logs | grep "negotiated protocol"
   ```
   - If you see `h2`, HTTP/2 is being used
   - If you see `http/1.1` or empty, HTTP/1.1 is being used

2. **If HTTP/2 is causing connection issues:**
   - The proxy will automatically fall back to HTTP/1.1 if the client doesn't negotiate `h2`
   - If a client negotiates `h2` but then fails, check for errors in the logs after the "negotiated protocol: h2" line
   - The `http2.Server` has a 30-second idle timeout - connections that hang longer than this will be closed

3. **Force HTTP/1.1 (if needed):**
   If you need to disable HTTP/2 support temporarily, you can modify `internal/proxy/handlers.go`:
   ```go
   NextProtos: []string{"http/1.1"}, // Only HTTP/1.1
   ```
   Then rebuild and redeploy the proxy.

**Note:** As of this writing, we have not observed any clients successfully negotiating HTTP/2 in production. All observed connections have used HTTP/1.1. This is likely because:
- Many package managers (npm, pip, etc.) use older HTTP client libraries that don't support HTTP/2
- Some clients may explicitly prefer HTTP/1.1 for compatibility reasons
- The proxy's CONNECT tunnel handling may influence protocol negotiation

If you do see `h2` in the logs, it means a client successfully negotiated HTTP/2, and the proxy should handle it correctly using the `http2.Server` implementation.

