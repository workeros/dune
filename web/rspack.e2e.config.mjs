import config from "./rspack.config.mjs";

export default {
  ...config,
  devServer: { ...config.devServer, hot: false, liveReload: false, client: false },
};
