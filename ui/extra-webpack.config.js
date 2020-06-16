let webpack = require('webpack');
let path = require('path');


module.exports = {
  plugins: [
    new webpack.ContextReplacementPlugin(/moment[\/\\]locale$/, /fr|en/)
  ],
  resolve: {
    alias: {
      "../../theme.config$": path.join(__dirname, "/semantic-ui/theme.config"),
      "../semantic-ui/site": path.join(__dirname, "/semantic-ui/site")
    }
  }
};