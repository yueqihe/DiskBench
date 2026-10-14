"use strict";

const { runGo } = require("./lib-go");

const status = runGo(["test", "./..."]);
process.exit(status);
