import { rspack } from "@rspack/core";
import { fileURLToPath } from "node:url";

export default {
  context: fileURLToPath(new URL(".", import.meta.url)),
  entry: "./src/main.tsx",
  target: "web",
  lazyCompilation: false,
  experiments: { css: true },
  output: { path: fileURLToPath(new URL("./dist", import.meta.url)), clean: true, publicPath: "auto", filename: "[name].[contenthash:16].js", cssFilename: "[name].[contenthash:16].css" },
  resolve: { extensions: [".tsx", ".ts", ".js"], alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) } },
  module: { rules: [
    { test: /\.tsx?$/, exclude: /node_modules/, loader: "builtin:swc-loader", options: { jsc: { parser: { syntax: "typescript", tsx: true }, transform: { react: { runtime: "automatic" } } } } },
    { test: /\.css$/, type: "css", use: [{ loader: "postcss-loader", options: { postcssOptions: { plugins: { "@tailwindcss/postcss": {} } } } }] },
  ] },
  plugins: [new rspack.HtmlRspackPlugin({ template: "./index.html" })],
  devServer: { host: "127.0.0.1", port: 5173, historyApiFallback: true, proxy: [{ context: ["/api"], target: "http://127.0.0.1:7443", ws: true }] },
};
