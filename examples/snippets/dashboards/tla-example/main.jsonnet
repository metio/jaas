local examplonet = import 'examplonet/main.libsonnet';

function(something="value", other="more", required)
  {
    person1: {
      welcome: 'Hello ' + something + '!',
      key: other,
      required: required,
    },
  } + examplonet
