local examplonet = import 'examplonet/main.libsonnet';

{
  person1: {
    name: examplonet.standard,
    welcome: 'Hello ' + self.name + '!',
  },
  person2: self.person1 { name: 'Bob' },

}
