package dashboard

// Launch tab script that spends the fragment ticket and keeps the credential in sessionStorage
const webappLaunchScript = "\n" +
	"const basePath = __BASE_PATH__;\n" +
	"const params = new URLSearchParams(location.hash.slice(1));\n" +
	"history.replaceState(null, '', location.pathname);\n" +
	"const ticket = params.get('ticket') || '';\n" +
	"const show = message => { document.getElementById('status').textContent = message; };\n" +
	"const fail = () => show('This launch link was already used or has expired. Run icebath webapp again for a new one.');\n" +
	"if (!/^[A-Za-z0-9_-]{43}$/.test(ticket)) {\n" +
	"  fail();\n" +
	"} else {\n" +
	"  fetch(`${basePath}/webapp/window-session`, {\n" +
	"    method: 'POST', credentials: 'same-origin', headers: {'X-Icebath-Window-Ticket': ticket}\n" +
	"  }).then(response => response.ok ? response.json() : Promise.reject()).then(({token}) => {\n" +
	"    if (typeof token !== 'string' || !/^[A-Za-z0-9_-]{43}$/.test(token)) throw new Error();\n" +
	"    try {\n" +
	"      sessionStorage.setItem(`icebath.webapp.session.v1:${JSON.stringify([location.origin, basePath])}`, token);\n" +
	"    } catch {\n" +
	"      show('This browser does not let the Webapp keep its sign-in in this tab. Allow site data for this '\n" +
	"        + 'address, then run icebath webapp again.');\n" +
	"      return;\n" +
	"    }\n" +
	"    const target = new URL(`${basePath}/`, location.origin);\n" +
	"    target.search = params.get('query') || '';\n" +
	"    location.replace(target.href);\n" +
	"  }).catch(fail);\n" +
	"}\n" +
	""

// Child window script that receives a one use ticket from its authorized parent tab
const webappWindowScript = "\n" +
	"const basePath = __BASE_PATH__;\n" +
	"const params = new URLSearchParams(location.hash.slice(1));\n" +
	"history.replaceState(null, '', location.pathname);\n" +
	"const id = params.get('channel') || '';\n" +
	"const status = document.getElementById('status');\n" +
	"if (!/^[a-f0-9-]{36}$/.test(id)) {\n" +
	"  status.textContent = 'Open this window from an authorized Hermes Webapp tab.';\n" +
	"} else {\n" +
	"  const channel = new BroadcastChannel(`icebath.webapp.window:${id}`);\n" +
	"  let receiving = false;\n" +
	"  let finished = false;\n" +
	"  const controller = new AbortController();\n" +
	"  const fail = message => {\n" +
	"    if (finished) return;\n" +
	"    finished = true;\n" +
	"    controller.abort();\n" +
	"    status.textContent = message;\n" +
	"    channel.postMessage({type: 'error'});\n" +
	"    channel.close();\n" +
	"    clearTimeout(timeout);\n" +
	"  };\n" +
	"  const timeout = setTimeout(() => fail('This window link expired. Open a new window from the original tab.'), 30000);\n" +
	"  channel.onmessage = async event => {\n" +
	"    if (receiving) return;\n" +
	"    if (event.data?.error) { fail(event.data.error); return; }\n" +
	"    const ticket = event.data?.ticket;\n" +
	"    if (typeof ticket !== 'string' || !/^[A-Za-z0-9_-]{43}$/.test(ticket)) return;\n" +
	"    receiving = true;\n" +
	"    try {\n" +
	"      const response = await fetch(`${basePath}/webapp/window-session`, {\n" +
	"        method: 'POST', credentials: 'same-origin',\n" +
	"        headers: {'X-Icebath-Window-Ticket': ticket}, signal: controller.signal\n" +
	"      });\n" +
	"      if (!response.ok) throw new Error('Window authorization expired. Open a new window from the original tab.');\n" +
	"      const {token} = await response.json();\n" +
	"      if (finished) return;\n" +
	"      if (typeof token !== 'string' || !/^[A-Za-z0-9_-]{43}$/.test(token)) throw new Error('Invalid window authorization.');\n" +
	"      sessionStorage.setItem(`icebath.webapp.session.v1:${JSON.stringify([location.origin, basePath])}`, token);\n" +
	"      const target = new URL(`${basePath}/`, location.origin);\n" +
	"      target.search = params.get('query') || '';\n" +
	"      target.hash = params.get('route') || '/';\n" +
	"      channel.postMessage({type: 'done'});\n" +
	"      finished = true;\n" +
	"      clearTimeout(timeout);\n" +
	"      channel.close();\n" +
	"      location.replace(target.href);\n" +
	"    } catch (error) { fail(error.message || 'Could not authorize this window. Open it again from the original tab.'); }\n" +
	"  };\n" +
	"  channel.postMessage({type: 'ready'});\n" +
	"}\n" +
	""
