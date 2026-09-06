import { rspack } from "@rspack/core";
import { fileURLToPath } from "node:url";

export default {
  context: fileURLToPath(new URL(".", import.meta.url)),
  entry: "./src/main.tsx",
  target: "web",
  experiments: { css: true },
  output: { path: fileURLToPath(new URL("./dist", import.meta.url)), clean: true, publicPath: "/" },
  resolve: { extensions: [".tsx", ".ts", ".js"], alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) } },
  module: { rules: [
    { test: /\.tsx?$/, exclude: /node_modules/, loader: "builtin:swc-loader", options: { jsc: { parser: { syntax: "typescript", tsx: true }, transform: { react: { runtime: "automatic" } } } } },
    { test: /\.css$/, type: "css", use: [{ loader: "postcss-loader", options: { postcssOptions: { plugins: { "@tailwindcss/postcss": {} } } } }] },
  ] },
  plugins: [new rspack.HtmlRspackPlugin({ template: "./index.html" }), new rspack.CopyRspackPlugin({ patterns: [{ from: "install.sh", to: "install.sh" }] })],
  devServer: { host: "127.0.0.1", port: 5173, historyApiFallback: true, proxy: [{ context: ["/api"], target: "http://127.0.0.1:7443", ws: true }] },
};
