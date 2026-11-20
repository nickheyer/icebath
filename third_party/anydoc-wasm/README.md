# anydoc-wasm

C ABI wrapper around the anydoc crate, compiled to WebAssembly and embedded by pkg/tools/anydoc.

## Build

```sh
rustup target add wasm32-wasip1
cd third_party/anydoc-wasm
cargo build --release --target wasm32-wasip1
cp target/wasm32-wasip1/release/anydoc_wasm.wasm ../../pkg/tools/anydoc/anydoc.wasm
```
