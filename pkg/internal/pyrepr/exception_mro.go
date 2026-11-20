package pyrepr

// Single base of each Python builtin and common stdlib exception class, Exception classes rooted at BaseException
var builtinExceptionParents = map[string]string{
	"Exception": "BaseException", "GeneratorExit": "BaseException", "KeyboardInterrupt": "BaseException",
	"SystemExit": "BaseException", "BaseExceptionGroup": "BaseException", "CancelledError": "BaseException",
	"ArithmeticError": "Exception", "FloatingPointError": "ArithmeticError", "OverflowError": "ArithmeticError",
	"ZeroDivisionError": "ArithmeticError", "AssertionError": "Exception", "AttributeError": "Exception",
	"BufferError": "Exception", "EOFError": "Exception", "ImportError": "Exception", "ModuleNotFoundError": "ImportError",
	"LookupError": "Exception", "IndexError": "LookupError", "KeyError": "LookupError", "MemoryError": "Exception",
	"NameError": "Exception", "UnboundLocalError": "NameError", "OSError": "Exception", "BlockingIOError": "OSError",
	"ChildProcessError": "OSError", "ConnectionError": "OSError", "BrokenPipeError": "ConnectionError",
	"ConnectionAbortedError": "ConnectionError", "ConnectionRefusedError": "ConnectionError",
	"ConnectionResetError": "ConnectionError", "FileExistsError": "OSError", "FileNotFoundError": "OSError",
	"InterruptedError": "OSError", "IsADirectoryError": "OSError", "NotADirectoryError": "OSError",
	"PermissionError": "OSError", "ProcessLookupError": "OSError", "TimeoutError": "OSError", "gaierror": "OSError",
	"herror": "OSError", "SSLError": "OSError", "ReferenceError": "Exception", "RuntimeError": "Exception",
	"NotImplementedError": "RuntimeError", "RecursionError": "RuntimeError", "PythonFinalizationError": "RuntimeError",
	"StopAsyncIteration": "Exception", "StopIteration": "Exception", "SyntaxError": "Exception",
	"IndentationError": "SyntaxError", "TabError": "IndentationError", "SystemError": "Exception", "TypeError": "Exception",
	"ValueError": "Exception", "UnicodeError": "ValueError", "UnicodeDecodeError": "UnicodeError",
	"UnicodeEncodeError": "UnicodeError", "UnicodeTranslateError": "UnicodeError", "JSONDecodeError": "ValueError",
	"SubprocessError": "Exception", "TimeoutExpired": "SubprocessError", "CalledProcessError": "SubprocessError",
	"Warning": "Exception", "UserWarning": "Warning", "DeprecationWarning": "Warning",
	"PendingDeprecationWarning": "Warning", "SyntaxWarning": "Warning", "RuntimeWarning": "Warning",
	"FutureWarning": "Warning", "ImportWarning": "Warning", "UnicodeWarning": "Warning", "BytesWarning": "Warning",
	"ResourceWarning": "Warning", "EncodingWarning": "Warning",
}

// Linearized bases of the builtin exceptions that inherit from two classes
var builtinExceptionMultiBases = map[string][]string{
	"ExceptionGroup":           {"BaseExceptionGroup", "Exception", "BaseException"},
	"SSLCertVerificationError": {"SSLError", "OSError", "ValueError", "Exception", "BaseException"},
}

// Class names from a builtin exception class through BaseException as type(exc).__mro__ lists them, nil for any other class
func BuiltinExceptionMRO(class string) []string {
	_, single := builtinExceptionParents[class]
	_, multi := builtinExceptionMultiBases[class]
	if !single && !multi && class != "BaseException" {
		return nil
	}
	return ExceptionMROFrom(class, nil)
}

// Class names from class up its base chain in parents then the builtin chain, an unknown root deriving from Exception
func ExceptionMROFrom(class string, parents map[string]string) []string {
	out := []string{class}
	for {
		if next, ok := parents[class]; ok {
			class = next
			out = append(out, class)
			continue
		}
		if bases, ok := builtinExceptionMultiBases[class]; ok {
			return append(out, bases...)
		}
		if next, ok := builtinExceptionParents[class]; ok {
			class = next
			out = append(out, class)
			continue
		}
		if class != "BaseException" {
			out = append(out, "Exception", "BaseException")
		}
		return out
	}
}
