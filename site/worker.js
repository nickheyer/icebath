const SCRIPT_CLIENT = /^(curl|wget|fetch|httpie|aria2|libfetch)\b/i
const POWERSHELL = /PowerShell/i
const PACKAGE = /^\/(?:apt\/pool|rpm)\/([^/]+\.(?:deb|rpm))$/
const APK = /^\/apk\/(x86_64|aarch64)\/icebath-[^/]+\.apk$/

// Serves the install script to terminals and redirects package downloads to the matching GitHub release asset
export default {
  async fetch(request, env) {
    const url = new URL(request.url)
    const release = `${env.RELEASES}/v${env.VERSION}`
    if (url.pathname === '/') {
      const agent = request.headers.get('user-agent') || ''
      if (POWERSHELL.test(agent)) return script(request, env, '/install.ps1')
      if (SCRIPT_CLIENT.test(agent)) return script(request, env, '/install.sh')
      const version = { element: (el) => el.setInnerContent(`v${env.VERSION}`) }
      return new HTMLRewriter().on('[data-version]', version).transform(await env.ASSETS.fetch(request))
    }
    const pkg = url.pathname.match(PACKAGE)
    if (pkg) return Response.redirect(`${release}/${pkg[1]}`, 302)
    const apk = url.pathname.match(APK)
    if (apk) return Response.redirect(`${release}/icebath_${env.VERSION}_${apk[1]}.apk`, 302)
    return env.ASSETS.fetch(request)
  },
}

// Fetches a script asset and labels it as plain text
async function script(request, env, path) {
  const res = await env.ASSETS.fetch(new URL(path, request.url))
  const headers = new Headers(res.headers)
  headers.set('content-type', 'text/plain; charset=utf-8')
  return new Response(res.body, { status: res.status, headers })
}
