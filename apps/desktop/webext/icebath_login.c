#include <gio/gio.h>
#include <gio/gunixsocketaddress.h>
#include <string.h>
#include <webkit2/webkit-web-extension.h>

/* Socket of the desktop process that answers the gateway header rule for each sign-in request */
static gchar *icebath_socket_path;

typedef struct {
	GString *query;
} IcebathHeaderWalk;

static void icebath_append_header(const char *name, const char *value, gpointer data) {
	IcebathHeaderWalk *walk = data;
	g_string_append_printf(walk->query, "%s\x1f%s\n", name, value);
}

/* Asks the desktop for the outgoing header set of a request, leaving the request unchanged on any failure */
static gboolean icebath_send_request(WebKitWebPage *page, WebKitURIRequest *request, WebKitURIResponse *redirected, gpointer user_data) {
	SoupMessageHeaders *headers = webkit_uri_request_get_http_headers(request);
	const char *uri = webkit_uri_request_get_uri(request);
	if (!icebath_socket_path || !uri || (strncmp(uri, "http:", 5) != 0 && strncmp(uri, "https:", 6) != 0)) {
		return FALSE;
	}
	IcebathHeaderWalk walk = {g_string_new(NULL)};
	g_string_append_printf(walk.query, "%" G_GUINT64_FORMAT "\n%s\n%s\n", webkit_web_page_get_id(page), uri,
		redirected ? webkit_uri_response_get_uri(redirected) : "");
	if (headers) {
		soup_message_headers_foreach(headers, icebath_append_header, &walk);
	}
	g_string_append_c(walk.query, '\n');
	GSocketClient *client = g_socket_client_new();
	GSocketAddress *address = g_unix_socket_address_new(icebath_socket_path);
	GSocketConnection *connection = g_socket_client_connect(client, G_SOCKET_CONNECTABLE(address), NULL, NULL);
	g_object_unref(address);
	g_object_unref(client);
	if (!connection) {
		g_string_free(walk.query, TRUE);
		return FALSE;
	}
	GOutputStream *out = g_io_stream_get_output_stream(G_IO_STREAM(connection));
	gboolean written = g_output_stream_write_all(out, walk.query->str, walk.query->len, NULL, NULL, NULL);
	g_string_free(walk.query, TRUE);
	if (!written) {
		g_object_unref(connection);
		return FALSE;
	}
	GDataInputStream *in = g_data_input_stream_new(g_io_stream_get_input_stream(G_IO_STREAM(connection)));
	gchar *first = g_data_input_stream_read_line(in, NULL, NULL, NULL);
	if (first && strcmp(first, "+") == 0 && headers) {
		soup_message_headers_clear(headers);
		gchar *line;
		while ((line = g_data_input_stream_read_line(in, NULL, NULL, NULL)) && *line) {
			gchar *sep = strchr(line, '\x1f');
			if (sep) {
				*sep = '\0';
				soup_message_headers_append(headers, line, sep + 1);
			}
			g_free(line);
		}
		g_free(line);
	}
	g_free(first);
	g_object_unref(in);
	g_object_unref(connection);
	return FALSE;
}

static void icebath_page_created(WebKitWebExtension *extension, WebKitWebPage *page, gpointer user_data) {
	g_signal_connect(page, "send-request", G_CALLBACK(icebath_send_request), NULL);
}

/* Entry point WebKit calls in each sign-in web process with the desktop socket path */
G_MODULE_EXPORT void webkit_web_extension_initialize_with_user_data(WebKitWebExtension *extension, const GVariant *user_data) {
	icebath_socket_path = g_variant_dup_string((GVariant *)user_data, NULL);
	g_signal_connect(extension, "page-created", G_CALLBACK(icebath_page_created), NULL);
}
