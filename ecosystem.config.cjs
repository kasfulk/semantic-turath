const path = require("path");

module.exports = {
  apps: [
    {
      name: "semantic-turath",
      script: path.join(__dirname, "semantic-turath"),
      args: "--serve 20052",
      cwd: __dirname,
      env: { PORT: "20052" },
      autorestart: true,
    },
  ],
};
